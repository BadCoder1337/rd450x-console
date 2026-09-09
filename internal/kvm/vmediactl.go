package kvm

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"rd450x-console/internal/kvm/vmedia"
	"rd450x-console/internal/webui"
)

// vmediaControl implements webui.VMediaController by reusing the live KVM client's
// web session: the token minted for the video session also authenticates virtual
// media, so no extra BMC web login is opened per attach (and the fragile MegaRAC web
// stack is not hit again). The session is released only when the KVM client closes.
// The client connects asynchronously, so it is published via setClient once the
// handshake completes; attaches before that report "not ready yet".
type vmediaControl struct {
	host         string
	mu           sync.Mutex
	cl           *Client
	activeMounts int
}

func newVMediaControl(host string) *vmediaControl { return &vmediaControl{host: host} }

// setClient publishes the connected KVM client whose web session vmedia reuses.
func (v *vmediaControl) setClient(c *Client) {
	v.mu.Lock()
	v.cl = c
	v.activeMounts = 0
	// Clear a slot left stale by an abruptly terminated previous console. This is
	// the same type-24 state=0 command JViewer sends from stopRedirection().
	if err := c.setMediaRedirectionState(false); err != nil {
		log.Printf("vmedia: clear initial media state: %v", err)
	}
	v.mu.Unlock()
}

// Attach opens the device's vmedia port using the KVM session's token and runs the
// SCSI emulation loop against backing in a goroutine. The returned Mount stops the
// loop on Close; the web session is not touched (it belongs to the KVM client).
func (v *vmediaControl) Attach(ctx context.Context, kind string, backing vmedia.ReadWriter, size int64, writable bool) (webui.Mount, error) {
	v.mu.Lock()
	cl := v.cl
	v.mu.Unlock()
	if cl == nil {
		return nil, fmt.Errorf("vmedia: %w — try again in a moment", vmedia.ErrNotReady)
	}
	token, cookie, args := cl.VMediaSession()
	if token == "" {
		return nil, fmt.Errorf("vmedia: no web session token available")
	}

	opts := vmedia.Options{
		Host:       v.host,
		DeviceType: vmedia.DeviceCDROM, // JViewer uses the CD header for FD/HD auth too; the port selects the device
		Instance:   0,
		Debug:      os.Getenv("RD450X_VMEDIA_DEBUG") != "",
	}

	var sess *vmedia.Session
	var err error
	if args["singleportenabled"] == "1" {
		sess, err = v.connectSinglePort(ctx, kind, token, cookie, args, opts)
	} else {
		opts.Port, err = vmediaPort(kind, args)
		if err == nil {
			opts.TLS = args["vmsecure"] == "1" // RD450X: vmsecure=0 ⇒ plaintext
			sess, err = vmedia.Connect(ctx, opts, token)
		}
	}
	if err != nil {
		return nil, err
	}
	if err := v.mountStarted(cl); err != nil {
		_ = sess.Close()
		return nil, err
	}

	emu, cache := buildEmulator(kind, backing, writable)

	mctx, mcancel := context.WithCancel(ctx)
	mt := &vmediaMount{
		cancel: mcancel, sess: sess, kind: kind, cache: cache,
		onClose: func() { v.mountStopped(cl) },
		done:    make(chan error, 1),
	}
	go func() {
		err := sess.Serve(mctx, emu)
		if err != nil && mctx.Err() == nil {
			log.Printf("vmedia: %s session ended: %v", kind, err)
		}
		_ = mt.Close()
		mt.done <- err
	}()

	return mt, nil
}

// mountStarted/mountStopped maintain MegaRAC's video-session media-state bit.
// The UI permits CD/FD/HD simultaneously, so state=0 is sent only after the last
// active data-plane session closes.
func (v *vmediaControl) mountStarted(cl *Client) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.cl != cl {
		return fmt.Errorf("vmedia: KVM session changed during attach")
	}
	if v.activeMounts == 0 {
		if err := cl.setMediaRedirectionState(true); err != nil {
			return fmt.Errorf("vmedia: announce mounted state: %w", err)
		}
	}
	v.activeMounts++
	return nil
}

func (v *vmediaControl) mountStopped(cl *Client) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.cl != cl || v.activeMounts == 0 {
		return
	}
	v.activeMounts--
	if v.activeMounts == 0 {
		if err := cl.setMediaRedirectionState(false); err != nil {
			log.Printf("vmedia: announce unmounted state: %v", err)
		}
	}
}

// connectSinglePort reproduces JViewer's SinglePortKVM.setHTTPConnect path:
// open a fresh connection to kvmport, CONNECT to websecureport, select the
// device service, then run the normal IUSB authentication over that stream.
func (v *vmediaControl) connectSinglePort(ctx context.Context, kind, token, cookie string, args map[string]string, opts vmedia.Options) (*vmedia.Session, error) {
	service, err := vmediaService(kind)
	if err != nil {
		return nil, err
	}
	tunnelPort := portOr(args["kvmport"], 443)
	webPort := portOr(args["websecureport"], tunnelPort)
	useTLS := args["kvmsecure"] == "1"
	opts.Port = tunnelPort
	opts.TLS = false // TLS, when used, is already established by this package.

	var conn net.Conn
	if useTLS {
		conn, err = dialOpenSSL(v.host, tunnelPort)
	} else {
		conn, err = dial(v.host, tunnelPort, false)
	}
	if err != nil {
		return nil, fmt.Errorf("vmedia: dial single-port service %s: %w", service, err)
	}
	if err := conn.SetDeadline(time.Now().Add(dialTimeout)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vmedia: set single-port handshake deadline: %w", err)
	}
	r, err := negotiateSinglePortService(conn, v.host, webPort, cookie, service)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("vmedia: %s tunnel: %w", service, err)
	}

	// Preserve any bytes buffered while consuming the tunnel response. The BMC
	// normally waits for IUSB auth before sending more, but keeping the reader is
	// cheap and prevents a coalesced packet from being lost.
	stream := &bufferedNetConn{Conn: conn, r: r}
	sess, err := vmedia.ConnectConn(ctx, stream, opts, token)
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		sess.Close()
		return nil, fmt.Errorf("vmedia: clear single-port handshake deadline: %w", err)
	}
	log.Printf("vmedia: MegaRAC single-port %s tunnel established on %s:%d (%s)", service, v.host, tunnelPort, tunnelSecurity(useTLS))
	return sess, nil
}

func vmediaService(kind string) (string, error) {
	switch strings.ToLower(kind) {
	case "cd":
		return "CDMEDIA", nil
	case "fd":
		return "FDMEDIA", nil
	case "hd":
		return "HDMEDIA", nil
	default:
		return "", fmt.Errorf("vmedia: unknown device kind %q", kind)
	}
}

type bufferedNetConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedNetConn) Read(p []byte) (int, error) { return c.r.Read(p) }

var _ io.ReadWriteCloser = (*bufferedNetConn)(nil)

// vmediaPort maps a device kind to its BMC port, preferring the jnlp value and
// falling back to the documented defaults.
func vmediaPort(kind string, args map[string]string) (int, error) {
	switch kind {
	case "cd":
		return portOr(args["cdport"], vmedia.PortCD), nil
	case "fd":
		return portOr(args["fdport"], vmedia.PortFD), nil
	case "hd":
		return portOr(args["hdport"], vmedia.PortHD), nil
	default:
		return 0, fmt.Errorf("vmedia: unknown device kind %q", kind)
	}
}

func portOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return def
}

// buildEmulator picks the SCSI profile and fronts the backing with a windowed LRU
// read cache (one /control round-trip per backing read, so coalescing the BMC's
// sequential ≤128 KiB reads into larger aligned fetches cuts latency). CD-ROM is
// always read-only; floppy/HD/USB are writable when the browser supplied a writable
// backing — the cache then writes through and invalidates touched windows. The cache
// is returned so the mount can log its hit rate on detach.
func buildEmulator(kind string, backing vmedia.ReadWriter, writable bool) (*vmedia.Device, *vmedia.Cache) {
	switch {
	case kind == "cd":
		cache := vmedia.NewCache(backing, nil)
		return vmedia.NewCDROM(cache), cache
	case writable:
		cache := vmedia.NewCache(backing, backing)
		return vmedia.NewDiskRW(cache), cache
	default:
		cache := vmedia.NewCache(backing, nil)
		return vmedia.NewDisk(cache), cache
	}
}

// vmediaMount is an active redirection. Close is idempotent and leaves the shared
// web session alone (the KVM client owns it).
type vmediaMount struct {
	cancel  context.CancelFunc
	sess    *vmedia.Session
	kind    string
	cache   *vmedia.Cache
	onClose func()
	done    chan error
	once    sync.Once
}

func (m *vmediaMount) Close() error {
	m.once.Do(func() {
		m.cancel()
		_ = m.sess.Close()
		if m.onClose != nil {
			m.onClose()
		}
		if m.cache != nil {
			s := m.cache.Stats()
			if total := s.Hits + s.Misses; total > 0 {
				log.Printf("vmedia: %s cache — %d hits / %d misses (%.0f%%), %d KiB fetched from browser",
					m.kind, s.Hits, s.Misses, 100*float64(s.Hits)/float64(total), s.FetchedBytes/1024)
			}
		}
	})
	return nil
}

func (m *vmediaMount) Done() <-chan error { return m.done }
