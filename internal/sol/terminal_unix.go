//go:build !windows

package sol

import (
	"os"

	"golang.org/x/term"
)

type unixTerminal struct {
	fd    int
	saved *term.State
}

func openTerminal() (terminal, error) {
	fd := int(os.Stdin.Fd())
	saved, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	t := &unixTerminal{fd: fd, saved: saved}
	if err := t.write(enterFullscreen); err != nil {
		_ = term.Restore(fd, saved)
		return nil, err
	}
	return t, nil
}

func (t *unixTerminal) read(buf []byte) (int, error) {
	return os.Stdin.Read(buf)
}

func (t *unixTerminal) write(b []byte) error {
	// Arrow keys already arrive as escape sequences on a POSIX tty, and the
	// terminal renders UTF-8 directly, so a straight write is enough.
	for len(b) > 0 {
		n, err := os.Stdout.Write(b)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

func (t *unixTerminal) close() error {
	_ = t.write(leaveFullscreen)
	return term.Restore(t.fd, t.saved)
}
