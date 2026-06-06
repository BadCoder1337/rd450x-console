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

// maxFrameBytes caps the reassembled video frame. A 1080p ASPEED frame is well
// under this; the bound just stops a malformed or hostile stream (huge h.Size,
// or an endless run of non-terminal fragments) from growing the buffer until it
// exhausts memory before the codec can reject it.
const maxFrameBytes = 8 << 20 // 8 MiB

// FrameFunc is called with each fully decoded video frame.
type FrameFunc func(f *codec.Frame)

// Client speaks the BMC's IVTP KVM protocol over one TCP/TLS socket.
type Client struct {
	conn net.Conn
	r    *bufio.Reader
	wmu  sync.Mutex // serializes writes
	dec  *codec.Decoder

	webHost   string // for releasing the web session on close
	webCookie string

	OnFrame FrameFunc // optional; invoked on each decoded frame
}

// Options configure a KVM connection.
type Options struct {
	Host string
	Port int  // video port, default 7582
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

	c := &Client{
		conn:      conn,
		r:         bufio.NewReaderSize(conn, 1<<16),
		dec:       codec.New(),
		webHost:   opts.Host,
		webCookie: sess.Cookie,
	}

	// Bound the handshake: after the TCP/TLS dial, a BMC that stops responding
	// mid-handshake would otherwise block this goroutine forever (the read loop
	// itself is unbounded by design — frames arrive continuously). Clear the
	// deadline once validated so the steady video stream is not time-limited.
	conn.SetDeadline(time.Now().Add(dialTimeout))
	if err := c.handshake(sess, opts.User); err != nil {
		conn.Close()
		Logout(opts.Host, sess.Cookie)
		return nil, err
	}
	conn.SetDeadline(time.Time{})

	// The web session has done its only job: minting the kvmtoken and allocating
	// the video session, which is now validated and resumed. Release it right away
	// rather than holding it for the whole KVM session — that way the web session
	// never outlives its usefulness, even if the process is later killed hard
	// (bypassing Close()). The video stream is authenticated by the IVTP session,
	// not the web cookie.
	Logout(opts.Host, sess.Cookie)
	c.webCookie = ""
	log.Printf("kvm: web session released (KVM session is up)")
	return c, nil
}

func (c *Client) handshake(sess WebSession, user string) error {
	// The BMC handshake is reactive: on connect the card first sends
	// SESSION_ACCEPTED (23) with the active-client list; only then does the client
	// reply with VALIDATE_VIDEO_SESSION (18), and the card returns the validate
	// response (19). Sending validate before 23 makes the card reject it with
	// status 3 (INVALID_VIDEO_SESSION_INFO).
	for {
		h, err := readHeader(c.r)
		if err != nil {
			return fmt.Errorf("handshake read: %w", err)
		}
		switch h.Type {
		case opSessionAccepted:
			if h.Size > 0 {
				if _, err := c.r.Discard(int(h.Size)); err != nil {
					return fmt.Errorf("read session-accepted body: %w", err)
				}
			}
			// With KVM reconnect enabled (oemfeatures & 32, which this BMC sets),
			// JViewer sends a bodyless CONNECTION_COMPLETE (58) before the validate.
			// Without it the card treats the validate as a reconnect with mismatched
			// info and replies status 3 ("Invalid Session Information To Reconnect").
			if err := c.sendHeader(opConnectionComplete, 0); err != nil {
				return fmt.Errorf("send connection-complete: %w", err)
			}
			ip, mac := localAddrInfo(c.conn)
			pkt := buildValidatePacket(sess.Token, ip, user, mac)
			if err := c.write(pkt); err != nil {
				return fmt.Errorf("send validate: %w", err)
			}

		case opValidateVideoResp:
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

		case opStopSession:
			return fmt.Errorf("BMC refused session (stop, status %d)", h.Status)

		default:
			if h.Size > 0 {
				if _, err := c.r.Discard(int(h.Size)); err != nil {
					return fmt.Errorf("skip handshake msg %d: %w", h.Type, err)
				}
			}
		}
	}
}

// Run drives the read loop and a keep-alive ticker until ctx is cancelled or the
// socket fails.
func (c *Client) Run(ctx context.Context) error {
	go c.keepAlive(ctx)
	defer c.Close()

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
			if start+dataLen > maxFrameBytes {
				return fmt.Errorf("video frame exceeds %d bytes", maxFrameBytes)
			}
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
			log.Printf("kvm: frame %d, %d bytes (decode failed: %v)", seq, len(frame), err)
		}
		return
	}
	if seq == 1 || seq%120 == 0 {
		log.Printf("kvm: decoded frame %d (%dx%d)", seq, f.W, f.H)
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

// Close tears down the socket and releases the BMC web session.
func (c *Client) Close() error {
	err := c.conn.Close()
	Logout(c.webHost, c.webCookie)
	c.webCookie = ""
	return err
}
