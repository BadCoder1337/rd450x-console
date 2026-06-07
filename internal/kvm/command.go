package kvm

import (
	"context"
	"flag"
	"log"

	"rd450x-console/internal/config"
	"rd450x-console/internal/kvm/codec"
	"rd450x-console/internal/power"
	"rd450x-console/internal/rfb"
	"rd450x-console/internal/webui"
)

// powerControl adapts *power.Controller to webui.ControlHandler, mapping the
// toolbar's string actions onto typed chassis-control commands.
type powerControl struct{ c *power.Controller }

func (p powerControl) Power(ctx context.Context, action string) error {
	return p.c.Do(ctx, power.Action(action))
}

func (p powerControl) PowerStatus(ctx context.Context) (bool, error) {
	return p.c.Status(ctx)
}

// RunCommand implements `rd450x-console kvm`.
//
// It starts a local web server hosting the embedded noVNC client and acts as the
// RFB (VNC) server behind it. When credentials are present it connects to the
// BMC, completes the IVTP handshake, decodes the ASPEED video stream into RGB
// frames for noVNC, and forwards browser keyboard/mouse events back as USB HID
// reports. Without credentials it serves an animated test pattern so the
// noVNC↔RFB pipeline is still verifiable end-to-end.
func RunCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("kvm", flag.ContinueOnError)
	host := fs.String("host", "", "BMC host (default: IPMI_HOST)")
	user := fs.String("user", "", "BMC user (default: IPMI_USER)")
	port := fs.Int("port", 7582, "KVM video port")
	useTLS := fs.Bool("tls", true, "wrap the video socket in TLS (kvmsecure)")
	listen := fs.String("listen", "127.0.0.1:6080", "local web server address")
	noBrowser := fs.Bool("no-browser", false, "do not open a browser automatically")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// A cancellable child context so a noVNC disconnect (or signal) tears down
	// both the web server and the BMC client — the latter's deferred Close()
	// releases the card's web session. Without this, closing the browser tab
	// would orphan the video/web session on the BMC.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	creds := config.Load(*host, *user)

	var (
		src     rfb.Source
		sink    rfb.Sink
		control webui.ControlHandler
	)

	if creds.Host != "" && creds.User != "" && creds.Password != "" {
		// Power control rides standard IPMI chassis control over RMCP+ (UDP 623),
		// independent of the KVM video port.
		control = powerControl{power.New(creds.Host, config.PortOr(623), creds.User, creds.Password)}
		// Real BMC: a dynamic FrameSource fed by decoded video, and a HID Sink
		// driving keyboard/mouse. Both are created up front so the BMC client's
		// OnFrame and the sink are wired before Run starts.
		const initW, initH = 1024, 768
		fsrc := rfb.NewFrameSource(initW, initH)
		// The BMC connects asynchronously, so the real HID sink isn't available
		// until then. A late-binding sink discards input until it is published,
		// avoiding a data race on the pointer.
		late := newLateSink()
		src = fsrc
		sink = late
		go connectBMC(ctx, Options{
			Host: creds.Host, Port: *port, TLS: *useTLS, User: creds.User,
		}, creds.Password, fsrc, late)
	} else {
		log.Printf("kvm: IPMI_USER/IPMI_PASSWORD not set — serving test pattern only")
		src = rfb.NewTestPattern(ctx, 1024, 768)
		sink = rfb.NopSink()
	}

	return webui.Serve(ctx, *listen, src, sink, !*noBrowser, cancel, control)
}

// connectBMC establishes the BMC session, wires decoded frames into fsrc and
// builds the HID sink, publishing it through late for the RFB server.
func connectBMC(ctx context.Context, opts Options, password string, fsrc *rfb.FrameSource, late *lateSink) {
	c, err := Connect(ctx, opts, password)
	if err != nil {
		log.Printf("kvm: BMC connect failed: %v", err)
		return
	}
	log.Printf("kvm: connected to BMC %s:%d, streaming video fragments", opts.Host, opts.Port)

	hid := NewSink(ctx, c, 1024, 768)
	c.OnFrame = func(f *codec.Frame) {
		fsrc.Update(f.W, f.H, f.Pix)
		hid.SetFrameSize(f.W, f.H)
	}
	late.set(hid)

	if err := c.Run(ctx); err != nil && ctx.Err() == nil {
		log.Printf("kvm: BMC session ended: %v", err)
	}
}
