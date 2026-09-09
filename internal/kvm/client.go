package kvm

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
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

	webHost          string // for releasing the web session on close
	webCookie        string
	webToken         string            // STOKEN, reused for virtual-media auth
	webArgs          map[string]string // parsed jnlp args (vmedia ports, vmsecure)
	legacySinglePort bool              // ESC8000 G3 JViewer dialect (no opcode-57 keepalive)
	frameSeen        atomic.Bool
	kmEncryption     atomic.Bool // server requires Blowfish-wrapped keyboard/mouse reports
	kmCrypt          *kmCrypt

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

	// FetchLaunchArgs (rather than Login) so we keep the full jnlp arg map: the
	// virtual-media path reuses this session's token and needs the per-device ports
	// and vmsecure flag carried in the same response.
	args, cookie, err := FetchLaunchArgs(ctx, opts.Host, opts.User, password)
	if err != nil {
		return nil, err
	}
	token := args["kvmtoken"]
	if token == "" {
		Logout(opts.Host, cookie)
		return nil, fmt.Errorf("launch jnlp: no -kvmtoken in response (parsed %d args)", len(args))
	}
	webcookie := args["webcookie"]
	if webcookie == "" {
		webcookie = cookie
	}
	sess := WebSession{Token: token, Cookie: webcookie}
	log.Printf("kvm: web session established (token %d bytes)", len(token))

	// Newer MegaRAC deployments often expose a dedicated IVTP socket (usually
	// 7582). This ASUS firmware instead advertises singleportenabled=1 and
	// multiplexes JViewer through its HTTPS port. Treat the dynamic JNLP as the
	// authority for transport selection.
	singlePort := args["singleportenabled"] == "1"
	if singlePort {
		if p, parseErr := strconv.Atoi(args["kvmport"]); parseErr == nil && p > 0 {
			opts.Port = p
		}
		opts.TLS = args["kvmsecure"] == "1"
	}

	var conn net.Conn
	useOpenSSL := singlePort && opts.TLS
	if useOpenSSL {
		conn, err = dialOpenSSL(opts.Host, opts.Port)
	} else {
		conn, err = dial(opts.Host, opts.Port, opts.TLS)
	}
	if err != nil {
		Logout(opts.Host, webcookie)
		return nil, fmt.Errorf("dial video port: %w", err)
	}
	// Bound both the proprietary HTTP upgrade and the IVTP validation. The
	// deadline is cleared once the streaming session is ready.
	if err := conn.SetDeadline(time.Now().Add(dialTimeout)); err != nil {
		conn.Close()
		Logout(opts.Host, webcookie)
		return nil, fmt.Errorf("set handshake deadline: %w", err)
	}

	var r *bufio.Reader
	if singlePort {
		webPort := opts.Port
		if p, parseErr := strconv.Atoi(args["websecureport"]); parseErr == nil && p > 0 {
			webPort = p
		}
		// The negotiation owns the buffered reader so bytes coalesced after the
		// HTTP response are handed on to the IVTP client rather than lost.
		r, err = negotiateSinglePort(conn, opts.Host, webPort, webcookie)
		if err != nil {
			conn.Close()
			Logout(opts.Host, webcookie)
			return nil, err
		}
		log.Printf("kvm: MegaRAC single-port tunnel established on %s:%d (%s)", opts.Host, opts.Port, tunnelSecurity(opts.TLS))
	} else {
		r = bufio.NewReaderSize(conn, 1<<16)
	}

	c := &Client{
		conn:             conn,
		r:                r,
		dec:              codec.New(),
		webHost:          opts.Host,
		webCookie:        webcookie,
		webToken:         token,
		webArgs:          args,
		legacySinglePort: singlePort,
	}
	if crypt, cryptErr := newKMCrypt(token); cryptErr != nil {
		// Video is still useful if a strange token cannot initialize Blowfish.
		// Should the BMC require KM encryption, input reports will fail clearly.
		log.Printf("kvm: keyboard/mouse encryption unavailable: %v", cryptErr)
	} else {
		c.kmCrypt = crypt
	}

	// Bound the handshake: after the TCP/TLS dial, a BMC that stops responding
	// mid-handshake would otherwise block this goroutine forever (the read loop
	// itself is unbounded by design — frames arrive continuously). Clear the
	// deadline once validated so the steady video stream is not time-limited.
	if err := c.handshake(sess, opts.User, singlePort); err != nil {
		conn.Close()
		Logout(opts.Host, webcookie)
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		Logout(opts.Host, webcookie)
		return nil, fmt.Errorf("clear handshake deadline: %w", err)
	}

	// Keep the web session alive. It minted the kvmtoken and allocated the video
	// session, but virtual media reuses that same token to authenticate its IUSB
	// redirection (internal/kvm/vmediactl.go), so we no longer release it after the
	// handshake. The session is logged out when the KVM client closes (Close),
	// tearing down video and virtual media together.
	log.Printf("kvm: web session retained until KVM disconnect")
	return c, nil
}

// VMediaSession returns the live web-session credentials and parsed jnlp arg
// map. Dedicated-port virtual media uses the token for IUSB auth; MegaRAC's
// single-port variant additionally uses the cookie for its HTTPS service tunnel.
func (c *Client) VMediaSession() (token, cookie string, args map[string]string) {
	return c.webToken, c.webCookie, c.webArgs
}

func (c *Client) handshake(sess WebSession, user string, legacySinglePort bool) error {
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
			if legacySinglePort {
				// The ESC8000 G3's JViewer sends both tokens and resumes immediately:
				// type 21 (web cookie), type 18 (KVM token), then type 6. Sending the
				// newer type 58 handshake makes this older BMC close the connection.
				webToken := append(header{Type: opWebSessionToken, Size: uint32(len(sess.Cookie))}.marshal(), []byte(sess.Cookie)...)
				if err := c.write(webToken); err != nil {
					return fmt.Errorf("send web-session token: %w", err)
				}
				ip, _ := localAddrInfo(c.conn)
				if err := c.write(buildLegacyValidatePacket(sess.Token, ip, user)); err != nil {
					return fmt.Errorf("send validate: %w", err)
				}
				if err := c.sendHeader(opResumeRedirection, 0); err != nil {
					return fmt.Errorf("send resume: %w", err)
				}
				continue
			}

			// Dedicated-port MegaRAC variants use the newer reconnect exchange.
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
			if !legacySinglePort {
				if err := c.sendHeader(opResumeRedirection, 0); err != nil {
					return fmt.Errorf("send resume: %w", err)
				}
			}
			// The legacy ASUS client requests a complete framebuffer after
			// validation (JViewer KVMClient.sendFullScreenRequest), rather than
			// waiting for the next changed-screen update.
			if legacySinglePort {
				if err := c.sendHeader(opGetFullScreen, 0); err != nil {
					return fmt.Errorf("request full screen: %w", err)
				}
			}
			return nil

		case opStopSession:
			return fmt.Errorf("BMC refused session (stop, status %d)", h.Status)

		case opInitialEncryption, opEncryptionStatus:
			body, err := c.readEncryptionStatus(h)
			if err != nil {
				return fmt.Errorf("read encryption status: %w", err)
			}
			c.enableKMEncryption(h, body)

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
	if !c.legacySinglePort {
		go c.keepAlive(ctx)
	}
	defer c.Close()

	errc := make(chan error, 1)
	go func() { errc <- c.readLoop() }()
	if c.legacySinglePort {
		go c.requestInitialFrame(ctx)
	}

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

		case opBlankScreen:
			log.Printf("kvm: BMC reports no video signal")
			if h.Size > 0 {
				if _, err := c.r.Discard(int(h.Size)); err != nil {
					return err
				}
			}

		case opInitialEncryption, opEncryptionStatus:
			body, err := c.readEncryptionStatus(h)
			if err != nil {
				return err
			}
			c.enableKMEncryption(h, body)

		case opGetMouseMode:
			body := make([]byte, h.Size)
			if _, err := io.ReadFull(c.r, body); err != nil {
				return err
			}
			if len(body) > 0 {
				log.Printf("kvm: BMC mouse mode %d (%s)", body[0], mouseModeName(body[0]))
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

// tunnelSecurity labels a single-port tunnel in logs so an operator can see at
// a glance whether keystrokes and media are crossing the wire in the clear.
func tunnelSecurity(tls bool) string {
	if tls {
		return "TLS via openssl"
	}
	return "PLAINTEXT"
}

// readEncryptionStatus consumes the (normally empty or one-byte) body of an
// ENCRYPTION_STATUS / INITIAL_ENCRYPTION_STATUS packet. It is capped so a
// corrupt size field cannot make us buffer a video frame's worth of bytes.
func (c *Client) readEncryptionStatus(h header) ([]byte, error) {
	const maxBody = 16
	if h.Size == 0 {
		return nil, nil
	}
	if h.Size > maxBody {
		if _, err := c.r.Discard(int(h.Size)); err != nil {
			return nil, err
		}
		return nil, nil
	}
	body := make([]byte, h.Size)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return nil, err
	}
	return body, nil
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
	c.frameSeen.Store(true)
	if c.OnFrame != nil {
		c.OnFrame(f)
	}
}

// requestInitialFrame repeats the older BMC's video-resume request after the
// read loop is active. Its initial resume is sent before the validation reply,
// exactly as JViewer does, but a busy card occasionally accepts the session
// without starting a static screen. Resume is idempotent, so retry briefly
// until the first frame decodes.
func (c *Client) requestInitialFrame(ctx context.Context) {
	for attempt := 0; attempt < 5 && !c.frameSeen.Load(); attempt++ {
		if err := c.sendHeader(opResumeRedirection, 0); err != nil {
			return
		}
		t := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
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

// setMediaRedirectionState mirrors JViewer's MediaRedirectionState command.
// The media data plane uses its own socket, but the ESC8000 G3's older MegaRAC
// firmware also tracks a global mounted/unmounted bit on the live IVTP video
// session and may impose a short cooldown before accepting another mount.
//
// It is only sent on the legacy single-port dialect. The RD450X's dedicated-port
// virtual media was verified without it, and no RD450X is available to confirm
// how its firmware treats an unsolicited type-24, so that path stays byte-for-byte
// as before.
func (c *Client) setMediaRedirectionState(mounted bool) error {
	if !c.legacySinglePort {
		return nil
	}
	var state uint16
	if mounted {
		state = 1
	}
	return c.sendHeader(opMediaState, state)
}

func (c *Client) write(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.conn.Write(b)
	return err
}

// closeGrace bounds the best-effort media-state write in Close. A peer that has
// stopped reading can leave another goroutine blocked inside conn.Write while it
// holds wmu; Close must not wait behind it, because closing the socket is what
// unblocks that writer in the first place.
const closeGrace = time.Second

// Close tears down the socket and releases the BMC web session.
func (c *Client) Close() error {
	// Best effort: release any virtual-media slot even if the browser or bridge
	// disappeared without running the normal mount cleanup path.
	done := make(chan struct{})
	go func() {
		_ = c.setMediaRedirectionState(false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(closeGrace):
		log.Printf("kvm: media-state release still blocked after %v; closing socket", closeGrace)
	}
	err := c.conn.Close()
	Logout(c.webHost, c.webCookie)
	c.webCookie = ""
	return err
}
