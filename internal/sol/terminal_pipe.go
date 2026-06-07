package sol

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

// pipeTerminal is the raw-passthrough terminal: it does no VT emulation and owns
// no screen model. The BMC's VT/ANSI byte stream is written straight to stdout
// and the host terminal renders it; raw stdin bytes flow straight back to the
// BMC. This is the ipmitool model.
//
// Because the host terminal does input encoding, F-keys / navigation come from
// it (the xterm-compatible sequences any modern terminal emits) rather than an
// in-process key table — the same contract every serial-console client lives by.
//
// Goroutine model: read() is called from Console's input goroutine (a blocking
// stdin read), write()/close() from the render goroutine. No internal locking is
// needed — read and write touch disjoint file handles, and Console serializes
// the writers.
type pipeTerminal struct {
	in        *os.File
	out       *os.File
	oldState  *term.State
	restoreVT func()
}

// openTerminal puts stdin into raw mode (no echo, no line buffering, no signal
// generation — every byte, including Ctrl-] and Ctrl-C, reaches us) and enables
// VT output on the host terminal. It requires a real terminal on both ends; a
// redirected stdin/stdout makes MakeRaw fail, which is reported as-is.
func openTerminal() (terminal, error) {
	in, out := os.Stdin, os.Stdout

	oldState, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return nil, fmt.Errorf("enter raw mode (the pipe terminal needs a real terminal): %w", err)
	}

	restoreVT, err := enableVTOutput()
	if err != nil {
		_ = term.Restore(int(in.Fd()), oldState)
		return nil, fmt.Errorf("enable terminal VT output: %w", err)
	}

	return &pipeTerminal{in: in, out: out, oldState: oldState, restoreVT: restoreVT}, nil
}

// read blocks for the next chunk of raw stdin. In raw mode the host terminal has
// already encoded keys to the bytes a serial console expects (arrow keys as VT
// sequences, Ctrl-<x> as their control byte), so no key table is involved and
// the bytes pass straight through Console's escape state machine. Returns io.EOF
// when stdin closes, which unwinds Console's input goroutine.
func (t *pipeTerminal) read(buf []byte) (int, error) {
	return t.in.Read(buf)
}

// write forwards server output to the host terminal verbatim. No UTF-8 boundary
// handling is needed: the terminal consumes the byte stream continuously, so a
// multi-byte rune split across two writes reassembles naturally.
func (t *pipeTerminal) write(b []byte) error {
	_, err := t.out.Write(b)
	return err
}

// close restores the host terminal: first the VT output mode, then cooked input.
func (t *pipeTerminal) close() error {
	if t.restoreVT != nil {
		t.restoreVT()
	}
	return term.Restore(int(t.in.Fd()), t.oldState)
}
