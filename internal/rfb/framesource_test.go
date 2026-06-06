package rfb

import "testing"

func TestFrameSourcePlaceholder(t *testing.T) {
	s := NewFrameSource(1024, 768)
	f := s.Frame()
	if f == nil {
		t.Fatal("placeholder frame must be non-nil before first Update")
	}
	if f.W != 1024 || f.H != 768 {
		t.Errorf("placeholder dims = %dx%d, want 1024x768", f.W, f.H)
	}
	if len(f.Pix) != 1024*768*4 {
		t.Errorf("placeholder pix len = %d, want %d", len(f.Pix), 1024*768*4)
	}
}

func TestFrameSourceUpdate(t *testing.T) {
	s := NewFrameSource(64, 64)
	pix := make([]byte, 800*600*4)
	s.Update(800, 600, pix)
	f := s.Frame()
	if f.W != 800 || f.H != 600 {
		t.Errorf("after update dims = %dx%d, want 800x600", f.W, f.H)
	}
	// Changed must have signalled.
	select {
	case <-s.Changed():
	default:
		t.Error("Update did not signal Changed()")
	}
}

func TestFrameSourceUpdateRejectsBadInput(t *testing.T) {
	s := NewFrameSource(64, 64)
	// Too-short pixel buffer must be ignored, leaving the placeholder intact.
	s.Update(800, 600, make([]byte, 10))
	f := s.Frame()
	if f.W != 64 || f.H != 64 {
		t.Errorf("bad update mutated frame to %dx%d", f.W, f.H)
	}
}

func TestFrameSourceCoalesce(t *testing.T) {
	s := NewFrameSource(64, 64)
	// Two updates with no consumer in between must not block (buffered, coalesced).
	s.Update(64, 64, make([]byte, 64*64*4))
	s.Update(64, 64, make([]byte, 64*64*4))
	select {
	case <-s.Changed():
	default:
		t.Error("expected at least one pending change signal")
	}
}
