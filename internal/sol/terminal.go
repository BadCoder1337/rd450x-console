package sol

// Cross-platform interactive console for a serial line.
//
// The serial side speaks a byte stream of VT/ANSI escape sequences. We follow
// the ipmitool model (terminal_pipe.go): put the host terminal into raw mode and
// forward bytes straight through, with no VT emulation and no screen model. The
// host terminal's own renderer — any modern xterm, or Windows Terminal/conhost
// via ConPTY — interprets the VT stream, and in raw mode it also encodes keys to
// the bytes a serial console expects. This keeps key handling and rendering where
// decades of terminals already do it, instead of carrying an in-process emulator.
//
// A terminal exposes:
//
//   - read(buf) – blocking read of pending input, already encoded to the bytes
//     the BMC expects (driven from a goroutine)
//   - write(b)  – render server output (write the raw byte stream to stdout)
//   - close()   – restore the host terminal
//
// terminal is the single abstraction Console depends on; openTerminal (in
// terminal_pipe.go) returns the raw-passthrough implementation.
type terminal interface {
	read(buf []byte) (int, error)
	write(b []byte) error
	close() error
}
