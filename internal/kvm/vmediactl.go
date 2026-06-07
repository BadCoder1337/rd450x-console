package kvm

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"

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
	host string
	mu   sync.Mutex
	cl   *Client
}

func newVMediaControl(host string) *vmediaControl { return &vmediaControl{host: host} }

// setClient publishes the connected KVM client whose web session vmedia reuses.
func (v *vmediaControl) setClient(c *Client) {
	v.mu.Lock()
	v.cl = c
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
		return nil, fmt.Errorf("vmedia: BMC session not ready yet — try again in a moment")
	}
	token, args := cl.VMediaSession()
	if token == "" {
		return nil, fmt.Errorf("vmedia: no web session token available")
	}

	port, err := vmediaPort(kind, args)
	if err != nil {
		return nil, err
	}

	sess, err := vmedia.Connect(ctx, vmedia.Options{
		Host:       v.host,
		Port:       port,
		DeviceType: vmedia.DeviceCDROM, // JViewer uses the CD header for FD/HD auth too; the port selects the device
		Instance:   0,
		TLS:        args["vmsecure"] == "1", // RD450X: vmsecure=0 ⇒ plaintext
	}, token)
	if err != nil {
		return nil, err
	}

	emu, cache := buildEmulator(kind, backing, writable)

	mctx, mcancel := context.WithCancel(ctx)
	go func() {
		if err := sess.Serve(mctx, emu); err != nil && mctx.Err() == nil {
			log.Printf("vmedia: %s session ended: %v", kind, err)
		}
	}()

	return &vmediaMount{cancel: mcancel, sess: sess, kind: kind, cache: cache}, nil
}

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
	cancel context.CancelFunc
	sess   *vmedia.Session
	kind   string
	cache  *vmedia.Cache
	once   sync.Once
}

func (m *vmediaMount) Close() error {
	m.once.Do(func() {
		m.cancel()
		_ = m.sess.Close()
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
