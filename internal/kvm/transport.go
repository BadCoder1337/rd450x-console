package kvm

import (
	"crypto/tls"
	"net"
	"strconv"
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
