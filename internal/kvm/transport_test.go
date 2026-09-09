package kvm

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"testing"
)

func TestNegotiateSinglePortService(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	const (
		host    = "192.0.2.10"
		cookie  = "web-session-cookie"
		service = "CDMEDIA"
	)
	want := "CONNECT " + host + ":443 HTTPS/1.1\n cookie " + cookie + "\r\n\r\n" +
		"JVIEWER " + service + " cookie " + cookie + "\r\n\r\n"
	serverErr := make(chan error, 1)
	go func() {
		got := make([]byte, len(want))
		if _, err := io.ReadFull(server, got); err != nil {
			serverErr <- err
			return
		}
		if string(got) != want {
			serverErr <- fmt.Errorf("request = %q, want %q", got, want)
			return
		}
		_, err := io.WriteString(server, "HTTP/1.1 200 OK\r\nX-MegaRAC: tunnel\r\n\r\n")
		serverErr <- err
	}()

	if _, err := negotiateSinglePortService(client, host, 443, cookie, service); err != nil {
		t.Fatalf("negotiate: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server: %v", err)
	}
}

// The BMC TLS config must keep offering the ECDHE suites the RD450X's dedicated
// video port negotiates while adding the RSA key-exchange suites the ESC8000 G3
// needs; both families must be present and nothing may be duplicated.
func TestLegacyCipherSuitesCoverBothFamilies(t *testing.T) {
	suites := legacyTLSConfig().CipherSuites
	seen := make(map[uint16]int)
	for _, id := range suites {
		seen[id]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("cipher suite %#04x listed %d times", id, n)
		}
	}
	for _, want := range []uint16{
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_RSA_WITH_AES_256_CBC_SHA,
	} {
		if seen[want] == 0 {
			t.Errorf("cipher suite %s missing", tls.CipherSuiteName(want))
		}
	}
}

func TestNegotiateSinglePortRejectsUnsafeService(t *testing.T) {
	if _, err := negotiateSinglePortService(nil, "192.0.2.10", 443, "cookie", "CDMEDIA\r\nBAD"); err == nil {
		t.Fatal("unsafe service was accepted")
	}
	if _, err := negotiateSinglePortService(nil, "192.0.2.10", 443, "cookie\r\nBAD", "CDMEDIA"); err == nil {
		t.Fatal("unsafe cookie was accepted")
	}
}
