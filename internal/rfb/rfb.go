// Package rfb implements a minimal RFB 3.8 (VNC) server that runs over any
// net.Conn — in particular a WebSocket adapted to net.Conn — so the embedded
// noVNC client can render frames produced by the KVM bridge and forward
// keyboard/mouse input back.
//
// Only what noVNC needs is implemented: security type None, a fixed 32-bpp BGRX
// pixel format, and Raw-encoded full-frame updates. Tight/ZRLE and dirty-rect
// optimization can be added later.
package rfb

// Frame is a 32-bpp BGRX (little-endian) framebuffer; len(Pix) == W*H*4.
type Frame struct {
	W, H int
	Pix  []byte
}

// Source supplies framebuffer content to the server.
type Source interface {
	// Frame returns the current framebuffer. The returned value must remain
	// valid and unmutated until the next call (callers may hold it briefly).
	Frame() *Frame
	// Changed yields a value whenever the framebuffer is updated. A single
	// consumer is assumed.
	Changed() <-chan struct{}
}

// Sink receives input events decoded from the RFB client.
type Sink interface {
	KeyEvent(keysym uint32, down bool)
	PointerEvent(x, y int, buttons uint8)
}

// nopSink ignores all input; useful before the HID path is wired up.
type nopSink struct{}

func (nopSink) KeyEvent(uint32, bool)      {}
func (nopSink) PointerEvent(int, int, uint8) {}

// NopSink returns a Sink that discards input.
func NopSink() Sink { return nopSink{} }
