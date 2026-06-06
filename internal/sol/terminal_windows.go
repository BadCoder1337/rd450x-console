//go:build windows

package sol

import (
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Console mode flags (wincon.h). Defined locally so the code does not depend on
// which subset golang.org/x/sys/windows happens to export.
const (
	enableProcessedInput    = 0x0001
	enableLineInput         = 0x0002
	enableEchoInput         = 0x0004
	enableWindowInput       = 0x0008 // reports buffer-resize events as input records
	enableMouseInput        = 0x0010 // reports mouse moves/clicks as input records
	enableQuickEditMode     = 0x0040 // click-drag selects text AND pauses output
	enableExtendedFlags     = 0x0080 // required for the QuickEdit/mouse bits to apply
	enableVirtualTermInput  = 0x0200
	enableVirtualTermOutput = 0x0004 // ENABLE_VIRTUAL_TERMINAL_PROCESSING
)

// We read keystrokes through the low-level ReadConsoleInputW API and act ONLY on
// key-down events, ignoring mouse / resize / focus records entirely. Mouse
// movement can never reach the BMC, regardless of console mode or whether the
// remote side
// turned on xterm mouse reporting. (The earlier ENABLE_MOUSE_INPUT-off approach
// was not enough — in VT-input mode the terminal still delivers mouse coordinates
// as input, which the shell then echoed as on-screen garbage.)
var (
	modkernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procReadConsoleInput = modkernel32.NewProc("ReadConsoleInputW")
)

const (
	keyEventType = 0x0001 // INPUT_RECORD.EventType == KEY_EVENT

	vkPrior  = 0x21 // Page Up
	vkNext   = 0x22 // Page Down
	vkEnd    = 0x23
	vkHome   = 0x24
	vkLeft   = 0x25
	vkUp     = 0x26
	vkRight  = 0x27
	vkDown   = 0x28
	vkInsert = 0x2D
	vkDelete = 0x2E
	vkF1     = 0x70
	vkF2     = 0x71
	vkF3     = 0x72
	vkF4     = 0x73
)

// inputRecord mirrors the Win32 INPUT_RECORD: a WORD EventType, padding to the
// union's 4-byte alignment, then a 16-byte union (sized by MOUSE_EVENT_RECORD).
type inputRecord struct {
	eventType uint16
	_         uint16
	event     [16]byte
}

// keyEventRecord mirrors KEY_EVENT_RECORD, overlaid on inputRecord.event.
type keyEventRecord struct {
	bKeyDown          int32
	wRepeatCount      uint16
	wVirtualKeyCode   uint16
	wVirtualScanCode  uint16
	unicodeChar       uint16
	dwControlKeyState uint32
}

// keyReps clamps a KEY_EVENT_RECORD repeat count to a sane [1, 1024] range
// (a held key auto-repeats; 0 is treated as one press).
func keyReps(n uint16) int {
	switch {
	case n < 1:
		return 1
	case n > 1024:
		return 1024
	default:
		return int(n)
	}
}

// specialKeyANSI maps a virtual-key code (for keys that carry no UnicodeChar) to
// the ANSI escape sequence a serial console expects.
func specialKeyANSI(vk uint16) []byte {
	switch vk {
	case vkUp:
		return []byte("\x1b[A")
	case vkDown:
		return []byte("\x1b[B")
	case vkRight:
		return []byte("\x1b[C")
	case vkLeft:
		return []byte("\x1b[D")
	case vkHome:
		return []byte("\x1b[H")
	case vkEnd:
		return []byte("\x1b[F")
	case vkPrior:
		return []byte("\x1b[5~")
	case vkNext:
		return []byte("\x1b[6~")
	case vkInsert:
		return []byte("\x1b[2~")
	case vkDelete:
		return []byte("\x1b[3~")
	case vkF1:
		return []byte("\x1bOP")
	case vkF2:
		return []byte("\x1bOQ")
	case vkF3:
		return []byte("\x1bOR")
	case vkF4:
		return []byte("\x1bOS")
	}
	return nil
}

type windowsTerminal struct {
	hin, hout windows.Handle
	savedIn   uint32
	savedOut  uint32
	leftover  []byte // incomplete trailing UTF-8 bytes held between writes
}

func openTerminal() (terminal, error) {
	hin, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil {
		return nil, err
	}
	hout, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return nil, err
	}

	t := &windowsTerminal{hin: hin, hout: hout}
	_ = windows.GetConsoleMode(hin, &t.savedIn)
	_ = windows.GetConsoleMode(hout, &t.savedOut)

	// Output: enable ANSI/VT so the server's escape sequences render.
	_ = windows.SetConsoleMode(hout, t.savedOut|enableVirtualTermOutput)

	// Input: raw, key-events-only. Clear the cooked-mode flags so nothing is
	// echoed or pre-chewed, disable QuickEdit so a stray click cannot freeze
	// output, and disable mouse/window-resize input so those never enter the
	// console's input queue. We deliberately do NOT set ENABLE_VIRTUAL_TERMINAL_INPUT:
	// in VT-input mode the terminal re-encodes mouse movement as input bytes that
	// our reader would forward to the BMC (the shell then echoes them as garbage).
	// Instead read() uses ReadConsoleInputW and acts on key-down events only.
	// ENABLE_EXTENDED_FLAGS must be set for the QuickEdit/mouse bits to take effect.
	inMode := t.savedIn &^ uint32(enableLineInput|enableEchoInput|enableProcessedInput|
		enableQuickEditMode|enableMouseInput|enableWindowInput|enableVirtualTermInput)
	inMode |= enableExtendedFlags
	_ = windows.SetConsoleMode(hin, inMode)

	if err := t.write(enterFullscreen); err != nil {
		t.restore()
		return nil, err
	}
	return t, nil
}

// read blocks for the next batch of console input records, then returns only the
// bytes from key-down events (typed characters and mapped special keys). Mouse,
// resize, focus and key-up records are dropped, so they can never reach the BMC.
// A batch containing no keystrokes returns (0, nil); the caller simply reads
// again. buf is the caller's scratch space (≥ a few hundred bytes).
func (t *windowsTerminal) read(buf []byte) (int, error) {
	var records [16]inputRecord
	var nread uint32
	r, _, callErr := procReadConsoleInput.Call(
		uintptr(t.hin),
		uintptr(unsafe.Pointer(&records[0])),
		uintptr(len(records)),
		uintptr(unsafe.Pointer(&nread)),
	)
	if r == 0 {
		return 0, callErr
	}

	out := appendKeyBytes(buf[:0], records[:], int(nread))
	return len(out), nil
}

// appendKeyBytes extracts the keystroke bytes from the first n input records,
// appending them to dst: typed characters (UTF-8) and mapped special keys from
// key-down events; mouse / resize / focus / key-up records are dropped. Pure, so
// it is unit-tested against synthetic records without a real console.
func appendKeyBytes(dst []byte, records []inputRecord, n int) []byte {
	for i := 0; i < n && i < len(records); i++ {
		rec := &records[i]
		if rec.eventType != keyEventType {
			continue // mouse / resize / focus / menu — never forwarded
		}
		ke := (*keyEventRecord)(unsafe.Pointer(&rec.event[0]))
		if ke.bKeyDown == 0 {
			continue // key-up
		}
		reps := keyReps(ke.wRepeatCount)
		if ch := ke.unicodeChar; ch != 0 {
			// A typed character (incl. control codes like Enter 0x0D, Ctrl-]
			// 0x1D): emit it as UTF-8.
			var tmp [utf8.UTFMax]byte
			n := utf8.EncodeRune(tmp[:], rune(ch))
			for j := 0; j < reps; j++ {
				dst = append(dst, tmp[:n]...)
			}
			continue
		}
		if seq := specialKeyANSI(ke.wVirtualKeyCode); seq != nil {
			for j := 0; j < reps; j++ {
				dst = append(dst, seq...)
			}
		}
	}
	return dst
}

func (t *windowsTerminal) write(b []byte) error {
	if len(t.leftover) > 0 {
		b = append(t.leftover, b...)
		t.leftover = nil
	}
	head, tail := completeUTF8(b)
	if len(tail) > 0 {
		// Copy: tail aliases the caller's (possibly reused) buffer.
		t.leftover = append([]byte(nil), tail...)
	}
	if len(head) == 0 {
		return nil
	}

	u16 := utf16.Encode([]rune(string(head)))
	for len(u16) > 0 {
		n := len(u16)
		if n > 8000 { // chunk large bursts to bound a single WriteConsoleW call
			n = 8000
		}
		var written uint32
		if err := windows.WriteConsole(t.hout, &u16[0], uint32(n), &written, nil); err != nil {
			return err
		}
		if written == 0 {
			written = uint32(n)
		}
		u16 = u16[written:]
	}
	return nil
}

func (t *windowsTerminal) close() error {
	_ = t.write(leaveFullscreen)
	t.restore()
	return nil
}

func (t *windowsTerminal) restore() {
	_ = windows.SetConsoleMode(t.hin, t.savedIn)
	_ = windows.SetConsoleMode(t.hout, t.savedOut)
}
