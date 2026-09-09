package kvm

import (
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// newTestOpensslConn builds an opensslConn without a child process. rd/wr are
// what the conn reads from and writes to; the caller keeps the other ends.
func newTestOpensslConn(rd io.ReadCloser, wr io.WriteCloser) *opensslConn {
	return &opensslConn{
		stdin:  wr,
		stdout: rd,
		local:  &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1)},
		remote: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 443},
	}
}

func assertTimeout(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("errors.Is(err, os.ErrDeadlineExceeded) = false for %v", err)
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("error is not a net.Error timeout: %v", err)
	}
	if strings.Contains(err.Error(), "|0") || strings.Contains(err.Error(), "|1") {
		t.Fatalf("error leaks the pipe name: %v", err)
	}
	if !strings.Contains(err.Error(), "192.0.2.10:443") {
		t.Fatalf("error does not name the BMC endpoint: %v", err)
	}
}

// io.Pipe ends are not *os.File, so this exercises the timer fallback that
// Windows anonymous pipes need: a blocked Read must return a timeout error.
func TestOpensslConnFallbackReadDeadline(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	c := newTestOpensslConn(pr, nopWriteCloser{})
	if err := c.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	start := time.Now()
	_, err := c.Read(make([]byte, 8))
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Read blocked %v despite fallback deadline", d)
	}
	assertTimeout(t, err)
}

// Clearing the deadline before it fires must leave the conn usable.
func TestOpensslConnFallbackDeadlineCleared(t *testing.T) {
	pr, pw := io.Pipe()
	c := newTestOpensslConn(pr, nopWriteCloser{})
	if err := c.SetReadDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if err := c.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear deadline: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	go func() { pw.Write([]byte("ok")); pw.Close() }()
	buf := make([]byte, 8)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "ok" {
		t.Fatalf("Read after cleared deadline = %q, %v", buf[:n], err)
	}
}

// Real os.Pipe ends do support deadlines on Unix; the error must still be
// translated so it names the endpoint rather than the pipe.
func TestOpensslConnOSPipeReadDeadline(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pw.Close()
	c := newTestOpensslConn(pr, nopWriteCloser{})
	if err := c.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		if errors.Is(err, os.ErrNoDeadline) {
			t.Skip("pipes without deadline support on this platform; fallback covered above")
		}
		t.Fatalf("SetReadDeadline: %v", err)
	}
	_, err = c.Read(make([]byte, 8))
	assertTimeout(t, err)
}

func TestOpensslConnEOFPassesThrough(t *testing.T) {
	pr, pw := io.Pipe()
	pw.Close()
	c := newTestOpensslConn(pr, nopWriteCloser{})
	if _, err := c.Read(make([]byte, 8)); err != io.EOF {
		t.Fatalf("Read after peer close = %v, want io.EOF", err)
	}
}

// After Close, I/O must report a closed-connection error, not a pipe error.
func TestOpensslConnCloseReportsClosed(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	c := newTestOpensslConn(pr, nopWriteCloser{})
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := c.Read(make([]byte, 8))
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Read after Close = %v, want net.ErrClosed", err)
	}
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }
