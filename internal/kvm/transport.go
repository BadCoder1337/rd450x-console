package kvm

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

const dialTimeout = 15 * time.Second

// dial opens the video socket to the BMC. When useTLS is set (kvmsecure=1) the
// connection is TLS-wrapped; the BMC presents a self-signed cert and old TLS, so
// verification is disabled and a low MinVersion is allowed (matching JViewer's
// trust-all SSLContext).
func dial(host string, port int, useTLS bool) (net.Conn, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	d := &net.Dialer{Timeout: dialTimeout}
	if !useTLS {
		return d.Dial("tcp", addr)
	}
	return tls.DialWithDialer(d, "tcp", addr, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // BMC self-signed cert, matches JViewer
		MinVersion:         tls.VersionTLS10,
	})
}

// tunnelHandshake opens the video service through a single-port BMC's web
// server, as JViewer's SinglePortKVM does: a CONNECT line and a JVIEWER line,
// both carrying the web session cookie, answered by an HTTP 200 status line.
// The line breaks are JViewer's, bare "\n" and all. The status line is read a
// byte at a time so no IVTP bytes are consumed.
func tunnelHandshake(conn net.Conn, host string, port int, useTLS bool, cookie string) error {
	proto := "HTTP/1.1"
	if useTLS {
		proto = "HTTPS/1.1"
	}
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	defer func() { _ = conn.SetDeadline(time.Time{}) }()
	req := "CONNECT " + net.JoinHostPort(host, strconv.Itoa(port)) + " " + proto + "\n cookie " + cookie + "\r\n\r\n" +
		"JVIEWER VIDEO cookie " + cookie + "\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return fmt.Errorf("single-port tunnel: %w", err)
	}
	var line []byte
	b := make([]byte, 1)
	for len(line) < 256 && !strings.HasSuffix(string(line), "\r\n") {
		if _, err := io.ReadFull(conn, b); err != nil {
			return fmt.Errorf("single-port tunnel: %w", err)
		}
		line = append(line, b[0])
	}
	status := strings.TrimSpace(string(line))
	if f := strings.Fields(status); len(f) < 2 || f[1] != "200" {
		return errors.New("single-port tunnel refused: " + status)
	}
	return nil
}
