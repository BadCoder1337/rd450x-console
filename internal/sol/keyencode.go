package sol

import "github.com/gdamore/tcell/v2"

// encodeKey turns a tcell key event into the bytes a serial console expects.
// tcell gives us one complete, cross-platform key model, so this is the only
// key table — no per-OS virtual-key handling. Control characters (incl. the
// Ctrl-] attention key) are emitted as their raw byte so Console's byte-level
// escape state machine keeps working unchanged.
//
// The escape sequences use the xterm/VT convention (F5+ as CSI ... ~). This
// matches what a Linux serial getty and a VT-UTF8/xterm BIOS console expect; a
// BIOS set to a different terminal emulation may key some F-keys differently.
func encodeKey(ev *tcell.EventKey) []byte {
	k := ev.Key()
	mod := ev.Modifiers()

	if k == tcell.KeyRune {
		r := ev.Rune()
		// Ctrl + ASCII letter that did not normalize to a control Key: fold to the
		// matching control byte (Ctrl-A → 0x01).
		if mod&tcell.ModCtrl != 0 && r >= 'a' && r <= 'z' {
			return altPrefix(mod, []byte{byte(r-'a') + 1})
		}
		if mod&tcell.ModCtrl != 0 && r >= 'A' && r <= 'Z' {
			return altPrefix(mod, []byte{byte(r-'A') + 1})
		}
		return altPrefix(mod, []byte(string(r)))
	}

	if seq := specialKeySeq(k); seq != nil {
		return seq
	}

	// tcell's "+64" control block (KeyCtrlSpace..KeyCtrlUnderscore, 64..95) maps
	// linearly to control bytes 0x00..0x1f.
	if k >= tcell.KeyCtrlSpace && k <= tcell.KeyCtrlUnderscore {
		return []byte{byte(k - tcell.KeyCtrlSpace)}
	}
	// The raw ASCII C0/DEL block: Enter (0x0D), Tab (0x09), Esc (0x1B), Ctrl-]
	// (0x1D), DEL (0x7F), and any control key tcell reports as its byte value.
	if k <= 0x7f {
		return []byte{byte(k)}
	}
	return nil
}

// specialKeySeq maps the named navigation/function keys to VT escape sequences.
func specialKeySeq(k tcell.Key) []byte {
	switch k {
	case tcell.KeyUp:
		return []byte("\x1b[A")
	case tcell.KeyDown:
		return []byte("\x1b[B")
	case tcell.KeyRight:
		return []byte("\x1b[C")
	case tcell.KeyLeft:
		return []byte("\x1b[D")
	case tcell.KeyHome:
		return []byte("\x1b[H")
	case tcell.KeyEnd:
		return []byte("\x1b[F")
	case tcell.KeyPgUp:
		return []byte("\x1b[5~")
	case tcell.KeyPgDn:
		return []byte("\x1b[6~")
	case tcell.KeyInsert:
		return []byte("\x1b[2~")
	case tcell.KeyDelete:
		return []byte("\x1b[3~")
	case tcell.KeyBacktab:
		return []byte("\x1b[Z")
	case tcell.KeyF1:
		return []byte("\x1bOP")
	case tcell.KeyF2:
		return []byte("\x1bOQ")
	case tcell.KeyF3:
		return []byte("\x1bOR")
	case tcell.KeyF4:
		return []byte("\x1bOS")
	case tcell.KeyF5:
		return []byte("\x1b[15~")
	case tcell.KeyF6:
		return []byte("\x1b[17~")
	case tcell.KeyF7:
		return []byte("\x1b[18~")
	case tcell.KeyF8:
		return []byte("\x1b[19~")
	case tcell.KeyF9:
		return []byte("\x1b[20~")
	case tcell.KeyF10:
		return []byte("\x1b[21~")
	case tcell.KeyF11:
		return []byte("\x1b[23~")
	case tcell.KeyF12:
		return []byte("\x1b[24~")
	}
	return nil
}

// altPrefix prepends ESC when Alt/Meta is held, the standard way a terminal
// sends Alt-<key>.
func altPrefix(mod tcell.ModMask, b []byte) []byte {
	if mod&tcell.ModAlt != 0 {
		return append([]byte{0x1b}, b...)
	}
	return b
}
