package rfb

import (
	"context"
	"sync"
	"time"
)

// TestPattern is a demo Source that renders an animated pattern. It exists to
// validate the noVNC ↔ RFB pipeline before the real ASPEED decoder feeds frames.
type TestPattern struct {
	w, h int

	mu    sync.Mutex
	frame *Frame
	tick  int

	changed chan struct{}
}

// NewTestPattern starts an animated w×h source updating ~10 times per second.
func NewTestPattern(ctx context.Context, w, h int) *TestPattern {
	tp := &TestPattern{w: w, h: h, changed: make(chan struct{}, 1)}
	tp.render()
	go tp.loop(ctx)
	return tp
}

func (tp *TestPattern) loop(ctx context.Context) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tp.mu.Lock()
			tp.tick++
			tp.render()
			tp.mu.Unlock()
			select {
			case tp.changed <- struct{}{}:
			default:
			}
		}
	}
}

// render builds a fresh frame (caller holds the lock, except for the initial call).
func (tp *TestPattern) render() {
	pix := make([]byte, tp.w*tp.h*4)
	off := (tp.tick * 4) % 256
	box := (tp.tick * 6) % (tp.w - 80)
	for y := 0; y < tp.h; y++ {
		for x := 0; x < tp.w; x++ {
			i := (y*tp.w + x) * 4
			// RGBX gradient (matches the negotiated noVNC pixel format)
			pix[i+0] = byte((x + y) & 0xff)         // R
			pix[i+1] = byte((y + off) & 0xff)       // G
			pix[i+2] = byte((x + off) & 0xff)       // B
			pix[i+3] = 0                            // X
			if x >= box && x < box+80 && y >= tp.h/2-40 && y < tp.h/2+40 {
				pix[i+0], pix[i+1], pix[i+2] = 255, 200, 0 // an orange box
			}
		}
	}
	tp.frame = &Frame{W: tp.w, H: tp.h, Pix: pix}
}

// Frame implements Source.
func (tp *TestPattern) Frame() *Frame {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return tp.frame
}

// Changed implements Source.
func (tp *TestPattern) Changed() <-chan struct{} { return tp.changed }
