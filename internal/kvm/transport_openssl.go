package kvm

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// opensslConn adapts an openssl s_client child process to net.Conn. This is a
// compatibility shim for old MegaRAC single-port firmware that accepts Java 8
// and OpenSSL TLS records but drops Go crypto/tls connections after the HTTP
// tunnel upgrade, despite negotiating the same TLS version and cipher.
//
// Deadlines: on Linux/macOS the stdio pipes support real deadlines. Where they
// do not (Windows anonymous pipes), SetDeadline falls back to a timer that tears
// the child down when it fires, so a blocked Read/Write still returns a timeout
// error instead of hanging forever. Callers close the conn after any handshake
// timeout anyway, so the coarser fallback has the same observable effect.
type opensslConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	local  net.Addr
	remote net.Addr
	once   sync.Once

	dmu          sync.Mutex // guards the fallback timers
	readTimer    *time.Timer
	writeTimer   *time.Timer
	readExpired  atomic.Bool
	writeExpired atomic.Bool
}

// opensslCiphers lists the TLS 1.2 suites offered to the BMC, in preference
// order. The ESC8000 G3 was verified with the first entry; the rest are the
// other RSA key-exchange suites this firmware generation is known to accept.
const opensslCiphers = "AES256-GCM-SHA384:AES128-GCM-SHA256:AES256-SHA:AES128-SHA"

func dialOpenSSL(host string, port int) (net.Conn, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	// -nocommands is essential: without it s_client treats any stdin chunk that
	// begins with 'Q', 'R', 'k' or 'K' as an interactive command (quit,
	// renegotiate, key update) and drops that chunk instead of sending it. It
	// reads stdin in 8 KiB chunks, so every large virtual-media response would
	// otherwise expose raw image bytes at chunk boundaries. Do not use -quiet
	// here: it implies -ign_eof, which would stop the stdin-EOF shutdown that
	// Close relies on for an orderly TLS close_notify.
	cmd := exec.Command("openssl", "s_client", "-brief", "-nocommands", "-tls1_2", "-cipher", opensslCiphers, "-connect", addr)
	// Keep the child out of the terminal's foreground process group. Otherwise a
	// Ctrl-C reaches openssl directly and kills it before our own shutdown path
	// can close its stdin, which turns the intended orderly close_notify into an
	// abrupt TCP teardown — exactly what leaves this firmware's media slot busy.
	configureChildProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("openssl stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("openssl stdout: %w", err)
	}
	// Drain diagnostics without copying certificate details and routine legacy
	// verification warnings into the console log.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("start openssl: %w", err)
	}
	remote, resolveErr := net.ResolveTCPAddr("tcp", addr)
	if resolveErr != nil {
		remote = &net.TCPAddr{Port: port}
	}
	local := &net.TCPAddr{}
	if udpRemote, udpErr := net.ResolveUDPAddr("udp", addr); udpErr == nil {
		if probe, probeErr := net.DialUDP("udp", nil, udpRemote); probeErr == nil {
			if a, ok := probe.LocalAddr().(*net.UDPAddr); ok {
				local.IP = a.IP
			}
			probe.Close()
		}
	}
	return &opensslConn{cmd: cmd, stdin: stdin, stdout: stdout, local: local, remote: remote}, nil
}

func (c *opensslConn) Read(p []byte) (int, error) {
	n, err := c.stdout.Read(p)
	return n, c.wrapErr("read", err, &c.readExpired)
}

func (c *opensslConn) Write(p []byte) (int, error) {
	n, err := c.stdin.Write(p)
	return n, c.wrapErr("write", err, &c.writeExpired)
}

func (c *opensslConn) LocalAddr() net.Addr  { return c.local }
func (c *opensslConn) RemoteAddr() net.Addr { return c.remote }

// wrapErr turns the child's pipe errors into net.OpError values that name the
// BMC endpoint instead of the pipe ("read |0: i/o timeout"), preserve
// errors.Is(err, os.ErrDeadlineExceeded) / net.Error.Timeout(), and report a
// fallback-timer teardown as the timeout it stands for.
func (c *opensslConn) wrapErr(op string, err error, expired *atomic.Bool) error {
	if err == nil {
		return nil
	}
	if expired.Load() {
		return c.opError(op, os.ErrDeadlineExceeded)
	}
	if errors.Is(err, io.EOF) {
		return io.EOF
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return c.opError(op, os.ErrDeadlineExceeded)
	}
	if errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return c.opError(op, net.ErrClosed)
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		return c.opError(op, pe.Err)
	}
	return c.opError(op, err)
}

func (c *opensslConn) opError(op string, err error) error {
	return &net.OpError{Op: op, Net: "tcp", Source: c.local, Addr: c.remote, Err: err}
}

func (c *opensslConn) Close() error {
	var err error
	c.once.Do(func() {
		c.stopTimers()
		// Closing stdin lets s_client drain bytes already written to the pipe,
		// send TLS close_notify, and close the TCP stream. Killing the helper first
		// produces a TCP reset, which can leave the BMC's virtual-media slot claimed
		// until its firmware timeout expires.
		_ = c.stdin.Close()
		if c.cmd == nil {
			_ = c.stdout.Close()
			return
		}
		done := make(chan error, 1)
		go func() { done <- c.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(time.Second):
			if c.cmd.Process != nil {
				err = c.cmd.Process.Kill()
			}
			<-done
		}
		_ = c.stdout.Close()
	})
	return err
}

func (c *opensslConn) SetDeadline(t time.Time) error {
	readErr := c.SetReadDeadline(t)
	writeErr := c.SetWriteDeadline(t)
	if readErr != nil {
		return readErr
	}
	return writeErr
}

func (c *opensslConn) SetReadDeadline(t time.Time) error {
	if f, ok := c.stdout.(*os.File); ok {
		if err := f.SetReadDeadline(t); !errors.Is(err, os.ErrNoDeadline) {
			return err
		}
	}
	c.armFallback(&c.readTimer, &c.readExpired, t)
	return nil
}

func (c *opensslConn) SetWriteDeadline(t time.Time) error {
	if f, ok := c.stdin.(*os.File); ok {
		if err := f.SetWriteDeadline(t); !errors.Is(err, os.ErrNoDeadline) {
			return err
		}
	}
	c.armFallback(&c.writeTimer, &c.writeExpired, t)
	return nil
}

// armFallback emulates a deadline on pipes that cannot carry one. A zero time
// clears it. When the timer fires it marks the direction expired and tears the
// child down, which is the only way to unblock a pending pipe Read/Write; the
// resulting error is reported as os.ErrDeadlineExceeded by wrapErr.
func (c *opensslConn) armFallback(timer **time.Timer, expired *atomic.Bool, t time.Time) {
	c.dmu.Lock()
	defer c.dmu.Unlock()
	if *timer != nil {
		(*timer).Stop()
		*timer = nil
	}
	expired.Store(false)
	if t.IsZero() {
		return
	}
	d := time.Until(t)
	if d < 0 {
		d = 0
	}
	*timer = time.AfterFunc(d, func() {
		expired.Store(true)
		c.teardown()
	})
}

func (c *opensslConn) stopTimers() {
	c.dmu.Lock()
	defer c.dmu.Unlock()
	for _, t := range []**time.Timer{&c.readTimer, &c.writeTimer} {
		if *t != nil {
			(*t).Stop()
			*t = nil
		}
	}
}

// teardown is the fallback deadline's action: kill the child and close both
// pipes so any blocked Read/Write returns immediately.
func (c *opensslConn) teardown() {
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.stdin.Close()
	_ = c.stdout.Close()
}
