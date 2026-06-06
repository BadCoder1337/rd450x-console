package sol

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

// TestEncodeKey is the cross-platform replacement for the old Windows-only
// appendKeyBytes test: it pins the tcell-event → BMC-byte mapping, including the
// keys the old hand-written table was missing (F5–F12) and the Ctrl-] attention
// key the escape state machine relies on.
func TestEncodeKey(t *testing.T) {
	cases := []struct {
		name string
		ev   *tcell.EventKey
		want string
	}{
		{"plain rune", tcell.NewEventKey(tcell.KeyRune, 'a', tcell.ModNone), "a"},
		{"enter", tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), "\r"},
		{"tab", tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone), "\t"},
		{"esc", tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone), "\x1b"},
		{"backspace", tcell.NewEventKey(tcell.KeyBackspace, 0, tcell.ModNone), "\x08"},

		{"up", tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone), "\x1b[A"},
		{"down", tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone), "\x1b[B"},
		{"home", tcell.NewEventKey(tcell.KeyHome, 0, tcell.ModNone), "\x1b[H"},
		{"delete", tcell.NewEventKey(tcell.KeyDelete, 0, tcell.ModNone), "\x1b[3~"},

		{"f1", tcell.NewEventKey(tcell.KeyF1, 0, tcell.ModNone), "\x1bOP"},
		{"f4", tcell.NewEventKey(tcell.KeyF4, 0, tcell.ModNone), "\x1bOS"},
		{"f5", tcell.NewEventKey(tcell.KeyF5, 0, tcell.ModNone), "\x1b[15~"},
		{"f11", tcell.NewEventKey(tcell.KeyF11, 0, tcell.ModNone), "\x1b[23~"},
		{"f12", tcell.NewEventKey(tcell.KeyF12, 0, tcell.ModNone), "\x1b[24~"},

		// Control keys: tcell normalizes Ctrl-<x> from a raw control rune.
		{"ctrl-a", tcell.NewEventKey(tcell.KeyRune, rune(0x01), tcell.ModNone), "\x01"},
		{"ctrl-] attention", tcell.NewEventKey(tcell.KeyRune, rune(DefaultEscape), tcell.ModNone), "\x1d"},
		{"ctrl-c", tcell.NewEventKey(tcell.KeyRune, rune(0x03), tcell.ModNone), "\x03"},

		// Alt-<rune> is ESC-prefixed.
		{"alt-x", tcell.NewEventKey(tcell.KeyRune, 'x', tcell.ModAlt), "\x1bx"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(encodeKey(c.ev)); got != c.want {
				t.Fatalf("encodeKey(%s) = %q, want %q", c.name, got, c.want)
			}
		})
	}
}

// TestEncodeKeyCtrlBlock checks the "+64" control block tcell sometimes reports
// (KeyCtrlRightSq etc.) folds to the same raw control byte as the ASCII form, so
// the attention key works regardless of which representation the platform path
// produces.
func TestEncodeKeyCtrlBlock(t *testing.T) {
	if got := encodeKey(tcell.NewEventKey(tcell.KeyCtrlRightSq, 0, tcell.ModCtrl)); len(got) != 1 || got[0] != 0x1d {
		t.Fatalf("KeyCtrlRightSq = %v, want [0x1d]", got)
	}
	if got := encodeKey(tcell.NewEventKey(tcell.KeyCtrlA, 0, tcell.ModCtrl)); len(got) != 1 || got[0] != 0x01 {
		t.Fatalf("KeyCtrlA = %v, want [0x01]", got)
	}
}
