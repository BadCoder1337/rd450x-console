package vmedia

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"
)

const dialTimeout = 15 * time.Second

// dialVmedia opens the vmedia socket. The BMC's jnlp advertises whether the media
// ports are TLS-wrapped via `vmsecure`: on the RD450X vmsecure=0, so virtual media
// is plaintext TCP even though the KVM video port (kvmsecure=1) is TLS. When
// useTLS is set we wrap with the same trust-all/low-MinVersion config as the video
// path (self-signed cert, old TLS).
func dialVmedia(host string, port int, useTLS bool) (net.Conn, error) {
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

// Session is one device redirection connection that has completed the IUSB auth
// handshake and is ready to serve SCSI requests.
type Session struct {
	conn       net.Conn
	deviceType uint8
	Debug      bool // hex-dump every request/response (protocol bring-up)
	closeOnce  sync.Once
	closeErr   error
}

// Options configures a redirection session.
type Options struct {
	Host       string
	Port       int   // PortCD / PortFD / PortHD
	DeviceType uint8 // DeviceCDROM, …
	Instance   uint8 // device slot (0 for the first CD/FD/HD)
	TLS        bool  // wrap in TLS (jnlp vmsecure=1); RD450X uses plaintext (0)
	Debug      bool
}

// Connect dials the vmedia port, sends the web-token auth packet, and waits for
// the BMC's redirection ACK. token is the web session STOKEN (kvm.WebSession.Token).
// On success the returned Session is ready for Serve.
func Connect(ctx context.Context, opts Options, token string) (*Session, error) {
	conn, err := dialVmedia(opts.Host, opts.Port, opts.TLS)
	if err != nil {
		return nil, fmt.Errorf("vmedia: dial %s:%d: %w", opts.Host, opts.Port, err)
	}
	if err := conn.SetDeadline(time.Now().Add(dialTimeout)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("vmedia: set authentication deadline: %w", err)
	}
	s, err := ConnectConn(ctx, conn, opts, token)
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("vmedia: clear authentication deadline: %w", err)
	}
	return s, nil
}

// ConnectConn authenticates an already-connected virtual-media stream. It is
// used by single-port MegaRAC clients after switching an HTTPS connection to a
// CDMEDIA/FDMEDIA/HDMEDIA service; Connect remains the dedicated-port entrypoint.
// ConnectConn takes ownership of conn and closes it on failure.
func ConnectConn(ctx context.Context, conn net.Conn, opts Options, token string) (*Session, error) {
	if conn == nil {
		return nil, fmt.Errorf("vmedia: nil connection")
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	s := &Session{conn: conn, deviceType: opts.DeviceType, Debug: opts.Debug}

	auth := buildAuth(opts.DeviceType, opts.Instance, token)
	if s.Debug {
		log.Printf("vmedia: → auth (%d bytes)\n%s", len(auth), hex.Dump(auth[:min(len(auth), 64)]))
	}
	if _, err := conn.Write(auth); err != nil {
		conn.Close()
		return nil, fmt.Errorf("vmedia: send auth: %w", err)
	}

	ack, err := s.readPacket()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("vmedia: read ACK: %w", err)
	}
	if s.Debug {
		log.Printf("vmedia: ← ACK opcode=0x%02X (%d payload bytes)\n%s",
			ack.Opcode(), len(ack.Payload), hex.Dump(ack.Payload[:min(len(ack.Payload), 64)]))
	}
	if err := ackStatus(ack); err != nil {
		_ = conn.Close()
		return nil, err
	}
	log.Printf("vmedia: redirection accepted (instance %d, port %d)", opts.Instance, opts.Port)
	return s, nil
}

// Handler emulates the device's SCSI/MMC command set: given a request packet it
// returns the response payload (the bytes after the IUSB header), or nil to send
// no reply. Returning an error tears the session down.
type Handler interface {
	Handle(req *Packet) (payload []byte, err error)
}

// Serve runs the request/response loop until the BMC sends a kill, the context is
// cancelled, or the connection drops.
func (s *Session) Serve(ctx context.Context, h Handler) error {
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()

	for {
		req, err := s.readPacket()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("vmedia: read request: %w", err)
		}
		if s.Debug {
			log.Printf("vmedia: ← req opcode=0x%02X seq=%d (%d bytes)\n%s",
				req.Opcode(), req.Header.SequenceNumber, len(req.Payload),
				hex.Dump(req.Payload[:min(len(req.Payload), 48)]))
		}
		if req.IsKill() {
			log.Printf("vmedia: BMC sent kill-redirection — closing")
			return nil
		}

		payload, err := h.Handle(req)
		if err != nil {
			return fmt.Errorf("vmedia: handler: %w", err)
		}
		if payload == nil {
			continue
		}
		if err := s.writeResponse(req, payload); err != nil {
			return fmt.Errorf("vmedia: write response: %w", err)
		}
	}
}

// Close terminates the session. The original ASUS JViewer's
// PacketMaster.VmediaSockclose method closes the single-port TLS socket without
// sending an IUSB command; the outer connection must therefore produce an
// orderly TLS close rather than killing its transport helper.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.conn.Close()
	})
	return s.closeErr
}

// readPacket reads one IUSB packet: the 32-byte header, then DataPacketLen
// payload bytes (the framing length lives at header offset 12, little-endian).
func (s *Session) readPacket() (*Packet, error) {
	hdr := make([]byte, HeaderLen)
	if _, err := io.ReadFull(s.conn, hdr); err != nil {
		return nil, err
	}
	h, err := parseHeader(hdr)
	if err != nil {
		return nil, err
	}
	if h.DataPacketLen > maxPacketPayload {
		return nil, fmt.Errorf("vmedia: implausible payload length %d", h.DataPacketLen)
	}
	payload := make([]byte, h.DataPacketLen)
	if _, err := io.ReadFull(s.conn, payload); err != nil {
		return nil, err
	}
	return &Packet{Header: h, Payload: payload}, nil
}

// writeResponse frames a response: an IUSB header (echoing the request's instance
// and sequence number, deviceType, direction=128, DataPacketLen=len(payload))
// followed by the payload.
func (s *Session) writeResponse(req *Packet, payload []byte) error {
	out := make([]byte, HeaderLen+len(payload))
	h := Header{
		Major:          iusbMajor,
		Minor:          iusbMinor,
		DataPacketLen:  uint32(len(payload)),
		DeviceType:     s.deviceType,
		Protocol:       1,
		Direction:      128,
		Instance:       req.Header.Instance,
		SequenceNumber: req.Header.SequenceNumber,
	}
	h.marshal(out)
	copy(out[HeaderLen:], payload)
	if s.Debug {
		log.Printf("vmedia: → resp seq=%d (%d payload bytes)\n%s",
			h.SequenceNumber, len(payload), hex.Dump(payload[:min(len(payload), 48)]))
	}
	_, err := s.conn.Write(out)
	return err
}

// maxPacketPayload caps a single packet at a little over the 128 KiB max transfer
// to guard against a corrupt length field wedging us into a huge allocation.
const maxPacketPayload = 0x20000 + 4096
