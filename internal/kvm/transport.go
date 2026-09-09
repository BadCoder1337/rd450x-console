package kvm

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

const dialTimeout = 15 * time.Second

// dial opens the video socket to the BMC. When useTLS is set (kvmsecure=1) the
// connection is TLS-wrapped. The BMC presents a self-signed legacy certificate,
// so verification is disabled to match JViewer's trust-all SSLContext.
func dial(host string, port int, useTLS bool) (net.Conn, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	d := &net.Dialer{Timeout: dialTimeout}
	if !useTLS {
		return d.Dial("tcp", addr)
	}
	return tls.DialWithDialer(d, "tcp", addr, legacyTLSConfig())
}

// legacyTLSConfig is intentionally scoped to connections made directly to the
// BMC. The ASUS negotiates TLS 1.2 with an RSA key-exchange cipher that recent Go
// releases no longer enable by default, while still older MegaRAC releases may
// require TLS 1.0. Certificate verification is disabled; callers should expose
// the client only on a trusted management path.
func legacyTLSConfig() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // legacy BMC self-signed certificate
		MinVersion:         tls.VersionTLS10,
		CipherSuites:       legacyCipherSuites(),
	}
}

// legacyCipherSuites is Go's current default list (ECDHE suites, which the
// RD450X's dedicated video port has always negotiated) extended with the plain
// RSA key-exchange suites that Go 1.22+ stopped offering by default and that the
// ESC8000 G3's HTTPS listener requires. Offering both keeps every BMC that worked
// before working, rather than swapping one family for the other.
func legacyCipherSuites() []uint16 {
	rsaKex := []uint16{
		tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,
	}
	seen := make(map[uint16]bool, 32)
	var out []uint16
	for _, cs := range tls.CipherSuites() {
		if !seen[cs.ID] {
			seen[cs.ID] = true
			out = append(out, cs.ID)
		}
	}
	for _, id := range rsaKex {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// negotiateSinglePort asks MegaRAC's HTTPS listener to switch this TLS
// connection from HTTP to the proprietary JViewer video service. The reader is
// returned to the IVTP client so any bytes coalesced after the HTTP response are
// preserved.
func negotiateSinglePort(conn net.Conn, host string, webPort int, cookie string) (*bufio.Reader, error) {
	return negotiateSinglePortService(conn, host, webPort, cookie, "VIDEO")
}

// negotiateSinglePortService switches one HTTPS connection to a named JViewer
// service. MegaRAC uses the same multiplexer for VIDEO and for the CDMEDIA,
// FDMEDIA, and HDMEDIA virtual-media data planes.
func negotiateSinglePortService(conn net.Conn, host string, webPort int, cookie, service string) (*bufio.Reader, error) {
	if cookie == "" {
		return nil, fmt.Errorf("single-port tunnel: empty web session cookie")
	}
	if strings.ContainsAny(host, "\r\n") || strings.ContainsAny(cookie, "\r\n") {
		return nil, fmt.Errorf("single-port tunnel: invalid host or cookie")
	}
	if webPort < 1 || webPort > 65535 {
		return nil, fmt.Errorf("single-port tunnel: invalid web port %d", webPort)
	}
	if service == "" || strings.ContainsAny(service, " \t\r\n") {
		return nil, fmt.Errorf("single-port tunnel: invalid service %q", service)
	}
	r := bufio.NewReaderSize(conn, 1<<16)
	// Match SinglePortKVM's request bytes exactly; this is not a standards-based
	// HTTP upgrade (notably, its request line ends in LF before the cookie line).
	authority := net.JoinHostPort(host, strconv.Itoa(webPort))
	connectRequest := fmt.Sprintf("CONNECT %s HTTPS/1.1\n cookie %s\r\n\r\n", authority, cookie)
	if _, err := io.WriteString(conn, connectRequest); err != nil {
		return nil, fmt.Errorf("single-port tunnel write: %w", err)
	}
	serviceRequest := fmt.Sprintf("JVIEWER %s cookie %s\r\n\r\n", service, cookie)
	// JViewer writes and flushes these separately. Keep the small spacing used by
	// the verified wire probe so the old HTTPS multiplexer sees the same stream.
	time.Sleep(50 * time.Millisecond)
	if _, err := io.WriteString(conn, serviceRequest); err != nil {
		return nil, fmt.Errorf("single-port service write: %w", err)
	}
	status, err := r.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("single-port tunnel response: %w", err)
	}
	fields := strings.Fields(status)
	if len(fields) < 2 || (fields[0] != "HTTP/1.0" && fields[0] != "HTTP/1.1") || fields[1] != "200" {
		return nil, fmt.Errorf("single-port tunnel rejected: %s", strings.TrimSpace(status))
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("single-port tunnel headers: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return r, nil
}
