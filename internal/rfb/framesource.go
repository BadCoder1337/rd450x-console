package rfb

import "sync"

// FrameSource is a concurrency-safe dynamic Source. The KVM read-loop goroutine
// calls Update on each decoded frame while the RFB server goroutine reads
// Frame()/Changed(); the two are guarded by a mutex and a coalescing signal.
//
// Before the first Update, Frame() returns a small black placeholder so the RFB
// ServerInit has valid dimensions.
type FrameSource struct {
	mu    sync.Mutex
	frame *Frame

	changed chan struct{}
}

// NewFrameSource returns a FrameSource pre-filled with a w×h black placeholder.
// w/h should be a sane initial guess (e.g. 1024×768); the real resolution
// follows from the first Update.
func NewFrameSource(w, h int) *FrameSource {
	if w <= 0 || h <= 0 {
		w, h = 640, 480
	}
	return &FrameSource{
		frame:   &Frame{W: w, H: h, Pix: make([]byte, w*h*4)},
		changed: make(chan struct{}, 1),
	}
}

// Update swaps in a new frame and signals consumers. pix is RGBX, len==w*h*4.
// The caller must not mutate pix after the call (the source takes ownership);
// the KVM codec allocates a fresh buffer per frame, so this is satisfied.
func (s *FrameSource) Update(w, h int, pix []byte) {
	if w <= 0 || h <= 0 || len(pix) < w*h*4 {
		return
	}
	s.mu.Lock()
	s.frame = &Frame{W: w, H: h, Pix: pix}
	s.mu.Unlock()
	select {
	case s.changed <- struct{}{}:
	default: // a signal is already pending; coalesce
	}
}

// Frame implements Source.
func (s *FrameSource) Frame() *Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frame
}

// Changed implements Source.
func (s *FrameSource) Changed() <-chan struct{} { return s.changed }
