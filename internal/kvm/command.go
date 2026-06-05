package kvm

import (
	"context"
	"flag"
	"log"

	"rd450x-console/internal/config"
	"rd450x-console/internal/rfb"
	"rd450x-console/internal/webui"
)

// RunCommand implements `rd450x-console kvm`.
//
// Current status (walking skeleton): it starts the web server with the embedded
// noVNC client and renders an animated test pattern so the noVNC↔RFB pipeline is
// verifiable end-to-end. In parallel, if credentials are present, it connects to
// the BMC and completes the IVTP handshake, logging the incoming video-fragment
// stream. Wiring decoded BMC frames into the RFB source lands with the codec.
func RunCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("kvm", flag.ContinueOnError)
	host := fs.String("host", "", "BMC host (default: IPMI_HOST or built-in)")
	user := fs.String("user", "", "BMC user (default: IPMI_USER)")
	port := fs.Int("port", 7582, "KVM video port")
	useTLS := fs.Bool("tls", true, "wrap the video socket in TLS (kvmsecure)")
	listen := fs.String("listen", "127.0.0.1:6080", "local web server address")
	noBrowser := fs.Bool("no-browser", false, "do not open a browser automatically")
	if err := fs.Parse(args); err != nil {
		return err
	}

	creds := config.Load(*host, *user)

	// Demo framebuffer until the decoder feeds real frames.
	src := rfb.NewTestPattern(ctx, 1024, 768)

	if creds.User != "" && creds.Password != "" {
		go connectBMC(ctx, Options{
			Host: creds.Host, Port: *port, TLS: *useTLS, User: creds.User,
		}, creds.Password)
	} else {
		log.Printf("kvm: IPMI_USER/IPMI_PASSWORD not set — serving test pattern only")
	}

	return webui.Serve(ctx, *listen, src, rfb.NopSink(), !*noBrowser)
}

func connectBMC(ctx context.Context, opts Options, password string) {
	c, err := Connect(ctx, opts, password)
	if err != nil {
		log.Printf("kvm: BMC connect failed: %v", err)
		return
	}
	log.Printf("kvm: connected to BMC %s:%d, streaming video fragments", opts.Host, opts.Port)
	if err := c.Run(ctx); err != nil && ctx.Err() == nil {
		log.Printf("kvm: BMC session ended: %v", err)
	}
}
