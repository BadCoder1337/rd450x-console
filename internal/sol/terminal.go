package sol

// Cross-platform raw terminal I/O for an interactive serial console.
//
// A terminal puts the local console into raw mode (no line buffering, no local
// echo) and exposes:
//
//   - read(buf) – blocking read of pending keystrokes (driven from a goroutine)
//   - write(b)  – render server output, including ANSI/VT escape sequences
//   - close()   – restore the original console mode
//
// On Windows, output goes through WriteConsoleW (UTF-16) with an incremental
// UTF-8 decoder so a multi-byte char (BIOS box-drawing) split across SOL packets
// is not mangled at a write boundary; special keys arrive pre-translated to ANSI
// because virtual-terminal *input* is enabled. On POSIX the tty is switched to
// raw mode with termios and stdin/stdout are used directly.

// Give a full-screen TUI (BIOS Setup, etc.) a clean canvas:
//
//	?1049h  switch to the alternate screen buffer (so the shell screen and
//	        scrollback are preserved and restored on exit),
//	2J/3J   clear the screen and scrollback,
//	H       home the cursor,
//	?7h     ensure auto-wrap is on (serial TUIs assume a normal terminal).
//
// Without this, the remote draws with absolute cursor addressing over whatever
// was already on screen, leaving stale cells everywhere.
var (
	enterFullscreen = []byte("\x1b[?1049h\x1b[2J\x1b[3J\x1b[H\x1b[?7h")
	leaveFullscreen = []byte("\x1b[?1049l")
)

// terminal is the platform-specific raw console. openTerminal returns the
// implementation for the current OS.
type terminal interface {
	read(buf []byte) (int, error)
	write(b []byte) error
	close() error
}

// completeUTF8 splits b into a prefix of complete UTF-8 sequences and a trailing
// remainder holding an incomplete multi-byte sequence (if any). The caller
// prepends the remainder to the next chunk so a code point split across two
// writes is rendered as one character rather than two replacement glyphs.
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
