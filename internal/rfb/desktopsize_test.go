package rfb

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"testing"
)

func TestWriteDesktopSize(t *testing.T) {
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	if err := writeDesktopSize(w, 1280, 1024); err != nil {
		t.Fatal(err)
	}
	w.Flush()
	b := buf.Bytes()
	if len(b) != 16 {
		t.Fatalf("desktopsize update len = %d, want 16", len(b))
	}
	if b[0] != 0 {
		t.Errorf("message type = %d, want 0 (FramebufferUpdate)", b[0])
	}
	if n := binary.BigEndian.Uint16(b[2:]); n != 1 {
		t.Errorf("num-rects = %d, want 1", n)
	}
	if x := binary.BigEndian.Uint16(b[4:]); x != 0 {
		t.Errorf("x = %d, want 0", x)
	}
	if ww := binary.BigEndian.Uint16(b[8:]); ww != 1280 {
		t.Errorf("w = %d, want 1280", ww)
	}
	if hh := binary.BigEndian.Uint16(b[10:]); hh != 1024 {
		t.Errorf("h = %d, want 1024", hh)
	}
	if enc := int32(binary.BigEndian.Uint32(b[12:])); enc != encDesktopSize {
		t.Errorf("encoding = %d, want %d (DesktopSize)", enc, encDesktopSize)
	}
}
