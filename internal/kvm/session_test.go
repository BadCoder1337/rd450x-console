package kvm

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func TestBuildLegacyValidatePacket(t *testing.T) {
	p := buildLegacyValidatePacket("0123456789abcdef", "192.0.2.10", "admin")
	if len(p) != LegacyVideoPacketSize {
		t.Fatalf("packet length = %d, want %d", len(p), LegacyVideoPacketSize)
	}
	if got := binary.LittleEndian.Uint16(p[0:2]); got != opValidateVideo {
		t.Fatalf("type = %d, want %d", got, opValidateVideo)
	}
	if got := binary.LittleEndian.Uint32(p[2:6]); got != LegacyVideoPacketSize {
		t.Fatalf("header size = %d, want %d", got, LegacyVideoPacketSize)
	}
	body := p[HeaderSize:]
	if body[0] != 0 || string(body[1:17]) != "0123456789abcdef" {
		t.Fatalf("unexpected token field")
	}
	if got := string(body[130 : 130+10]); got != "192.0.2.10" {
		t.Fatalf("IP field = %q", got)
	}
	if got := string(body[195 : 195+5]); got != "admin" {
		t.Fatalf("username field = %q", got)
	}
}

func TestLegacySinglePortHandshake(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	serverErr := make(chan error, 1)
	go func() {
		if _, err := serverConn.Write(header{Type: opSessionAccepted}.marshal()); err != nil {
			serverErr <- err
			return
		}

		webCookie := "0123456789abcdefghijklmnopqrstuvwxyz"
		webPacket := make([]byte, HeaderSize+len(webCookie))
		if _, err := io.ReadFull(serverConn, webPacket); err != nil {
			serverErr <- err
			return
		}
		if got := binary.LittleEndian.Uint16(webPacket[0:2]); got != opWebSessionToken {
			serverErr <- fmt.Errorf("web-token type = %d", got)
			return
		}
		if got := binary.LittleEndian.Uint32(webPacket[2:6]); got != uint32(len(webCookie)) {
			serverErr <- fmt.Errorf("web-token size = %d", got)
			return
		}
		if got := string(webPacket[HeaderSize:]); got != webCookie {
			serverErr <- fmt.Errorf("web-token payload mismatch")
			return
		}

		validate := make([]byte, LegacyVideoPacketSize)
		if _, err := io.ReadFull(serverConn, validate); err != nil {
			serverErr <- err
			return
		}
		if got := binary.LittleEndian.Uint16(validate[0:2]); got != opValidateVideo {
			serverErr <- fmt.Errorf("validate type = %d", got)
			return
		}
		if got := binary.LittleEndian.Uint32(validate[2:6]); got != LegacyVideoPacketSize {
			serverErr <- fmt.Errorf("validate size = %d", got)
			return
		}

		resume := make([]byte, HeaderSize)
		if _, err := io.ReadFull(serverConn, resume); err != nil {
			serverErr <- err
			return
		}
		if got := binary.LittleEndian.Uint16(resume[0:2]); got != opResumeRedirection {
			serverErr <- fmt.Errorf("resume type = %d", got)
			return
		}

		resp := append(header{Type: opValidateVideoResp, Size: 2}.marshal(), 1, 7)
		if _, err := serverConn.Write(resp); err != nil {
			serverErr <- err
			return
		}

		fullScreen := make([]byte, HeaderSize)
		if _, err := io.ReadFull(serverConn, fullScreen); err != nil {
			serverErr <- err
			return
		}
		if got := binary.LittleEndian.Uint16(fullScreen[0:2]); got != opGetFullScreen {
			serverErr <- fmt.Errorf("post-validation type = %d, want %d", got, opGetFullScreen)
			return
		}
		serverErr <- nil
	}()

	c := &Client{conn: clientConn, r: bufio.NewReader(clientConn)}
	sess := WebSession{Token: "0123456789abcdef", Cookie: "0123456789abcdefghijklmnopqrstuvwxyz"}
	if err := c.handshake(sess, "admin", true); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server side: %v", err)
	}
}

// The dedicated-port (RD450X) dialect must not emit the type-24 media-state
// packet at all; that path was verified without it.
func TestMediaRedirectionStateSkippedOnDedicatedPort(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	c := &Client{conn: clientConn}
	if err := c.setMediaRedirectionState(true); err != nil {
		t.Fatalf("setMediaRedirectionState: %v", err)
	}
	serverConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	buf := make([]byte, 1)
	if n, err := serverConn.Read(buf); err == nil || n != 0 {
		t.Fatalf("dedicated-port client wrote %d bytes, want none", n)
	}
}

// Close must not hang behind a writer that is stuck holding the write mutex on
// a peer that stopped reading; it has to close the socket within closeGrace.
func TestCloseDoesNotWaitForStuckWriter(t *testing.T) {
	clientConn, serverConn := net.Pipe() // unbuffered: writes block until read
	defer serverConn.Close()
	c := &Client{conn: clientConn, legacySinglePort: true}

	stuck := make(chan error, 1)
	go func() { stuck <- c.write(make([]byte, 64)) }() // nobody reads → blocks holding wmu
	time.Sleep(20 * time.Millisecond)

	start := time.Now()
	_ = c.Close()
	if d := time.Since(start); d > closeGrace+500*time.Millisecond {
		t.Fatalf("Close took %v, want ≤ %v", d, closeGrace)
	}
	select {
	case err := <-stuck:
		if err == nil {
			t.Fatal("stuck writer completed without error after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("stuck writer was not released by Close")
	}
}

func TestMediaRedirectionStateHeader(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	c := &Client{conn: clientConn, legacySinglePort: true}

	for _, tc := range []struct {
		mounted bool
		status  uint16
	}{{true, 1}, {false, 0}} {
		errCh := make(chan error, 1)
		go func(mounted bool) { errCh <- c.setMediaRedirectionState(mounted) }(tc.mounted)

		packet := make([]byte, HeaderSize)
		if _, err := io.ReadFull(serverConn, packet); err != nil {
			t.Fatalf("read media-state packet: %v", err)
		}
		if got := binary.LittleEndian.Uint16(packet[0:2]); got != opMediaState {
			t.Errorf("type = %d, want %d", got, opMediaState)
		}
		if got := binary.LittleEndian.Uint32(packet[2:6]); got != 0 {
			t.Errorf("size = %d, want 0", got)
		}
		if got := binary.LittleEndian.Uint16(packet[6:8]); got != tc.status {
			t.Errorf("status = %d, want %d", got, tc.status)
		}
		if err := <-errCh; err != nil {
			t.Fatalf("setMediaRedirectionState: %v", err)
		}
	}
}
