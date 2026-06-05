package kvm

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"rd450x-console/internal/kvm/codec"
)

const keepAliveInterval = 5 * time.Second

// FrameFunc is called with each fully decoded video frame.
type FrameFunc func(f *codec.Frame)

// Client speaks the BMC's IVTP KVM protocol over one TCP/TLS socket.
type Client struct {
	conn net.Conn
	r    *bufio.Reader
	wmu  sync.Mutex // serializes writes
	dec  *codec.Decoder

	OnFrame FrameFunc // optional; invoked on each decoded frame
}

// Options configure a KVM connection.
type Options struct {
	Host string
	Port int // video port, default 7582
	TLS  bool // kvmsecure
	User string
}

// Connect logs into the BMC web UI, opens the video socket, and completes the
// IVTP handshake (validate → resume). The password is used only for the web
// login and is never stored or logged.
func Connect(ctx context.Context, opts Options, password string) (*Client, error) {
	if opts.Port == 0 {
		opts.Port = 7582
	}

	sess, err := Login(ctx, opts.Host, opts.User, password)
	if err != nil {
		return nil, err
	}
	log.Printf("kvm: web session established (token %d bytes)", len(sess.Token))

	conn, err := dial(opts.Host, opts.Port, opts.TLS)
	if err != nil {
		return nil, fmt.Errorf("dial video port: %w", err)
	}

	c := &Client{conn: conn, r: bufio.NewReaderSize(conn, 1<<16), dec: codec.New()}

	if err := c.handshake(sess, opts.User); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) handshake(sess WebSession, user string) error {
	ip, mac := localAddrInfo(c.conn)
	pkt := buildValidatePacket(sess.Token, ip, user, mac)
	if err := c.write(pkt); err != nil {
		return fmt.Errorf("send validate: %w", err)
	}

	h, err := readHeader(c.r)
	if err != nil {
		return fmt.Errorf("read validate response: %w", err)
	}
	if h.Type != opValidateVideoResp {
		return fmt.Errorf("unexpected reply type %d (want %d)", h.Type, opValidateVideoResp)
	}
	body := make([]byte, h.Size)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return fmt.Errorf("read validate body: %w", err)
	}
	if len(body) == 0 || body[0] != 1 { // 1 = VALID_SESSION
		return fmt.Errorf("session rejected by BMC (status %v)", body)
	}
	log.Printf("kvm: video session validated")

	if err := c.sendHeader(opResumeRedirection, 0); err != nil {
		return fmt.Errorf("send resume: %w", err)
	}
	return nil
}

// Run drives the read loop and a keep-alive ticker until ctx is cancelled or the
// socket fails.
func (c *Client) Run(ctx context.Context) error {
	go c.keepAlive(ctx)
	defer c.conn.Close()

	errc := make(chan error, 1)
	go func() { errc <- c.readLoop() }()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errc:
		return err
	}
}

func (c *Client) readLoop() error {
	var frame []byte
	seq := 0
	for {
		h, err := readHeader(c.r)
		if err != nil {
			return err
		}
		switch h.Type {
		case opVideoFragment:
			if h.Size < 2 {
				if _, err := c.r.Discard(int(h.Size)); err != nil {
					return err
				}
				continue
			}
			var fn [2]byte
			if _, err := io.ReadFull(c.r, fn[:]); err != nil {
				return err
			}
			fragNum := binary.LittleEndian.Uint16(fn[:])
			dataLen := int(h.Size) - 2

			if fragNum&0x7fff == 0 { // first fragment of a frame
				frame = frame[:0]
			}
			start := len(frame)
			frame = append(frame, make([]byte, dataLen)...)
			if _, err := io.ReadFull(c.r, frame[start:]); err != nil {
				return err
			}

			if fragNum&0x8000 != 0 { // last fragment → frame complete
				seq++
				c.handleFrame(seq, frame)
				frame = frame[:0]
			}

		default:
			if h.Size > 0 {
				if _, err := c.r.Discard(int(h.Size)); err != nil {
					return err
				}
			}
			// TODO(kvm): dispatch control messages (power, LED, encryption...).
		}
	}
}

func (c *Client) handleFrame(seq int, frame []byte) {
	f, err := c.dec.Decode(frame)
	if err != nil {
		if seq == 1 || seq%60 == 0 {
			log.Printf("kvm: frame %d, %d bytes (decode pending: %v)", seq, len(frame), err)
		}
		return
	}
	if c.OnFrame != nil {
		c.OnFrame(f)
	}
}

func (c *Client) keepAlive(ctx context.Context) {
	t := time.NewTicker(keepAliveInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.sendHeader(opKeepAlive, 0); err != nil {
				return
			}
		}
	}
}

// sendHeader sends a bodyless IVTP packet.
func (c *Client) sendHeader(typ, status uint16) error {
	return c.write(header{Type: typ, Status: status}.marshal())
}

func (c *Client) write(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.conn.Write(b)
	return err
}

// Close tears down the socket.
func (c *Client) Close() error { return c.conn.Close() }
