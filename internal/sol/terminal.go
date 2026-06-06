package sol

// Cross-platform interactive console for a serial line.
//
// The serial side speaks a byte stream of VT/ANSI escape sequences. Rather than
// pass that stream straight to the user's real terminal (the old per-OS raw-mode
// approach), we drive a tcell.Screen and feed the serial stream through an
// embedded VT emulator (terminal_tcell.go): tcell owns input decoding and the
// physical screen on every platform, so F-keys / navigation / modifiers come
// from a single, complete event model instead of a hand-written Windows key
// table, and mouse/resize noise can never reach the BMC because we simply never
// enable the mouse.
//
// A terminal exposes:
//
//   - read(buf) – blocking read of pending input, already encoded to the bytes
//     the BMC expects (driven from a goroutine)
//   - write(b)  – fold server output into the emulator and repaint the screen
//   - close()   – tear the screen down and restore the console
//
// terminal is the single abstraction Console depends on; openTerminal returns
// the tcell-backed implementation.
type terminal interface {
	read(buf []byte) (int, error)
	write(b []byte) error
	close() error
}

// completeUTF8 splits b into a prefix of complete UTF-8 sequences and a trailing
// remainder holding an incomplete multi-byte sequence (if any). The caller
// prepends the remainder to the next chunk so a code point split across two
// writes (e.g. a BIOS box-drawing char spanning two SOL packets) is decoded as
// one rune. This must run before bytes reach the VT emulator: vt10x's Write
// drops the leading byte(s) of an incomplete trailing rune rather than holding
// them back, so without this a split multi-byte char is corrupted.
func completeUTF8(b []byte) (head, tail []byte) {
	// Scan back over at most the last 4 bytes for the start of the final rune.
	for i := 1; i <= 4 && i <= len(b); i++ {
		c := b[len(b)-i]
		if c < 0x80 {
			return b, nil // trailing byte is plain ASCII → all complete
		}
		if c&0xC0 == 0xC0 { // a lead byte: work out how many bytes it needs
			need := 0
			switch {
			case c&0xE0 == 0xC0:
				need = 2
			case c&0xF0 == 0xE0:
				need = 3
			case c&0xF8 == 0xF0:
				need = 4
			}
			if i < need { // started but not yet complete → hold it back
				return b[:len(b)-i], b[len(b)-i:]
			}
			return b, nil
		}
		// else: a continuation byte (10xxxxxx); keep scanning toward its lead.
	}
	return b, nil
}
