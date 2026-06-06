//go:build windows

package sol

import (
	"bytes"
	"testing"
	"unsafe"
)

func mkKey(down bool, vk, ch, reps uint16) inputRecord {
	var rec inputRecord
	rec.eventType = keyEventType
	ke := (*keyEventRecord)(unsafe.Pointer(&rec.event[0]))
	if down {
		ke.bKeyDown = 1
	}
	ke.wRepeatCount = reps
	ke.wVirtualKeyCode = vk
	ke.unicodeChar = ch
	return rec
}

func mkMouse() inputRecord {
	var rec inputRecord
	rec.eventType = 0x0002 // MOUSE_EVENT
	for i := range rec.event {
		rec.event[i] = 0x39 // junk that would render as '9' digits if forwarded
	}
	return rec
}

// TestAppendKeyBytesFiltersMouse is the guard for the mouse-garbage fix: only
// key-down characters and mapped special keys may be forwarded; mouse, key-up,
// and other records must be dropped so mouse movement never reaches the BMC.
func TestAppendKeyBytesFiltersMouse(t *testing.T) {
	recs := []inputRecord{
		mkKey(true, 0, 'h', 1),
		mkMouse(),               // dropped
		mkKey(true, 0, 'i', 1),  //
		mkKey(false, 0, 'X', 1), // key-up: dropped
		mkMouse(),               // dropped
		mkKey(true, vkUp, 0, 1), // Up arrow -> ESC [ A
		mkKey(true, 0, 0x0D, 1), // Enter
	}
	got := appendKeyBytes(nil, recs, len(recs))
	want := []byte("hi\x1b[A\r")
	if !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}

	// Repeat count is honored (a held key auto-repeats).
	if got := appendKeyBytes(nil, []inputRecord{mkKey(true, 0, 'a', 3)}, 1); string(got) != "aaa" {
		t.Fatalf("repeat: got %q, want %q", got, "aaa")
	}
}

// TestInputRecordLayout pins the struct layout to the Win32 ABI; a mismatch would
// make the unsafe overlay read garbage key codes (and break all keyboard input).
func TestInputRecordLayout(t *testing.T) {
	if got := unsafe.Sizeof(inputRecord{}); got != 20 {
		t.Errorf("inputRecord size = %d, want 20", got)
	}
	if got := unsafe.Sizeof(keyEventRecord{}); got != 16 {
		t.Errorf("keyEventRecord size = %d, want 16", got)
	}
	if got := unsafe.Offsetof(inputRecord{}.event); got != 4 {
		t.Errorf("inputRecord.event offset = %d, want 4", got)
	}
}
