package rfb

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync/atomic"
)

// client→server message types
const (
	msgSetPixelFormat           = 0
	msgSetEncodings             = 2
	msgFramebufferUpdateRequest = 3
	msgKeyEvent                 = 4
	msgPointerEvent             = 5
	msgClientCutText            = 6
)

// encDesktopSize is the DesktopSize pseudo-encoding; when the client advertises
// it in SetEncodings we may change the framebuffer dimensions mid-session.
const encDesktopSize int32 = -223

// Serve runs the RFB protocol on conn until ctx is cancelled or the connection
// ends. src provides frames; sink receives input (may be nil).
func Serve(ctx context.Context, conn net.Conn, src Source, sink Sink) error {
	if sink == nil {
		sink = NopSink()
	}
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)

	if err := handshake(r, w, src); err != nil {
		return err
	}

	// Track the dimensions last advertised to the client so we can emit a
	// DesktopSize rect when the BMC switches resolution mid-session.
	f0 := src.Frame()
	lastW, lastH := f0.W, f0.H

	// desktopSizeOK is set by the reader goroutine if the client advertised the
	// DesktopSize pseudo-encoding; read by this goroutine.
	var desktopSizeOK atomic.Bool

	// Reader goroutine: parse client messages, push update requests to reqc.
	reqc := make(chan bool, 1) // value = incremental
	errc := make(chan error, 1)
	go readMessages(r, sink, reqc, errc, &desktopSizeOK)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errc:
			return err
		case incremental := <-reqc:
			if incremental {
				// Block until the framebuffer changes (or we're done), so we
				// don't spin. noVNC won't request again until it's served.
				select {
				case <-ctx.Done():
					return ctx.Err()
				case err := <-errc:
					return err
				case <-src.Changed():
				}
			}
			f := src.Frame()
			if (f.W != lastW || f.H != lastH) && desktopSizeOK.Load() {
				if err := writeDesktopSize(w, f.W, f.H); err != nil {
					return err
				}
			}
			lastW, lastH = f.W, f.H
			if err := writeFrame(w, f); err != nil {
				return err
			}
		}
	}
}

// writeDesktopSize emits a FramebufferUpdate carrying a single DesktopSize
// pseudo-encoding rect (encoding -223) and no pixel data, telling the client the
// framebuffer is now newW×newH. It must precede the next Raw update at the new
// size.
func writeDesktopSize(w *bufio.Writer, newW, newH int) error {
	var hdr [16]byte
	hdr[0] = 0                             // FramebufferUpdate
	binary.BigEndian.PutUint16(hdr[2:], 1) // number-of-rectangles
	binary.BigEndian.PutUint16(hdr[4:], 0) // x
	binary.BigEndian.PutUint16(hdr[6:], 0) // y
	binary.BigEndian.PutUint16(hdr[8:], uint16(newW))
	binary.BigEndian.PutUint16(hdr[10:], uint16(newH))
	enc := encDesktopSize
	binary.BigEndian.PutUint32(hdr[12:], uint32(enc)) // encoding (-223)
	_, err := w.Write(hdr[:])
	return err
}

func handshake(r *bufio.Reader, w *bufio.Writer, src Source) error {
	// ProtocolVersion
	if _, err := w.WriteString("RFB 003.008\n"); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	var ver [12]byte
	if _, err := io.ReadFull(r, ver[:]); err != nil {
		return fmt.Errorf("read client version: %w", err)
	}

	// Security: offer only None (type 1).
	if _, err := w.Write([]byte{1, 1}); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	var sec [1]byte
	if _, err := io.ReadFull(r, sec[:]); err != nil {
		return fmt.Errorf("read security choice: %w", err)
	}
	// SecurityResult = OK (0)
	if err := writeU32(w, 0); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// ClientInit (shared-flag), ignored.
	var ci [1]byte
	if _, err := io.ReadFull(r, ci[:]); err != nil {
		return fmt.Errorf("read ClientInit: %w", err)
	}

	// ServerInit
	f := src.Frame()
	if err := writeServerInit(w, f.W, f.H, "RD450X KVM"); err != nil {
		return err
	}
	return w.Flush()
}

// writeServerInit sends dimensions, the fixed RGBX pixel format, and a name.
func writeServerInit(w *bufio.Writer, width, height int, name string) error {
	var b [24]byte
	binary.BigEndian.PutUint16(b[0:], uint16(width))
	binary.BigEndian.PutUint16(b[2:], uint16(height))
	// PixelFormat (16 bytes): 32 bpp, depth 24, little-endian, true-colour,
	// max 255 each, shifts R=0 G=8 B=16 → in-memory bytes [R,G,B,X]. This is the
	// exact format noVNC requests via SetPixelFormat (RFB.messages.pixelFormat),
	// so its raw decoder copies our bytes straight into the RGBA canvas without a
	// channel reorder — declaring BGRX here makes noVNC swap R↔B.
	b[4] = 32                               // bits-per-pixel
	b[5] = 24                               // depth
	b[6] = 0                                // big-endian-flag = false
	b[7] = 1                                // true-colour-flag = true
	binary.BigEndian.PutUint16(b[8:], 255)  // red-max
	binary.BigEndian.PutUint16(b[10:], 255) // green-max
	binary.BigEndian.PutUint16(b[12:], 255) // blue-max
	b[14] = 0                               // red-shift
	b[15] = 8                               // green-shift
	b[16] = 16                              // blue-shift
	// b[17..19] padding
	binary.BigEndian.PutUint32(b[20:], uint32(len(name)))
	if _, err := w.Write(b[:]); err != nil {
		return err
	}
	_, err := w.WriteString(name)
	return err
}

// writeFrame sends one FramebufferUpdate carrying the whole frame as a single
// Raw-encoded rectangle.
func writeFrame(w *bufio.Writer, f *Frame) error {
	if f == nil || f.W == 0 || f.H == 0 {
		return nil
	}
	var hdr [16]byte
	hdr[0] = 0 // FramebufferUpdate
	// hdr[1] padding
	binary.BigEndian.PutUint16(hdr[2:], 1) // number-of-rectangles
	binary.BigEndian.PutUint16(hdr[4:], 0) // x
	binary.BigEndian.PutUint16(hdr[6:], 0) // y
	binary.BigEndian.PutUint16(hdr[8:], uint16(f.W))
	binary.BigEndian.PutUint16(hdr[10:], uint16(f.H))
	binary.BigEndian.PutUint32(hdr[12:], 0) // encoding = Raw
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.Write(f.Pix); err != nil {
		return err
	}
	return w.Flush()
}

func readMessages(r *bufio.Reader, sink Sink, reqc chan<- bool, errc chan<- error, desktopSizeOK *atomic.Bool) {
	for {
		typ, err := r.ReadByte()
		if err != nil {
			errc <- err
			return
		}
		switch typ {
		case msgSetPixelFormat:
			if _, err := discard(r, 19); err != nil { // 3 pad + 16 pixel-format
				errc <- err
				return
			}
			// noVNC's default matches our ServerInit format; ignored.
		case msgSetEncodings:
			var b [3]byte
			if _, err := io.ReadFull(r, b[:]); err != nil {
				errc <- err
				return
			}
			n := int(binary.BigEndian.Uint16(b[1:]))
			// Read the encoding list so we can detect DesktopSize (-223).
			// Pixel encodings are ignored — we always send Raw, which every
			// client supports.
			for i := 0; i < n; i++ {
				var e [4]byte
				if _, err := io.ReadFull(r, e[:]); err != nil {
					errc <- err
					return
				}
				if int32(binary.BigEndian.Uint32(e[:])) == encDesktopSize {
					desktopSizeOK.Store(true)
				}
			}
		case msgFramebufferUpdateRequest:
			var b [9]byte
			if _, err := io.ReadFull(r, b[:]); err != nil {
				errc <- err
				return
			}
			incremental := b[0] != 0
			select {
			case reqc <- incremental:
			default: // a request is already pending; coalesce
			}
		case msgKeyEvent:
			var b [7]byte
			if _, err := io.ReadFull(r, b[:]); err != nil {
				errc <- err
				return
			}
			down := b[0] != 0
			keysym := binary.BigEndian.Uint32(b[3:])
			sink.KeyEvent(keysym, down)
		case msgPointerEvent:
			var b [5]byte
			if _, err := io.ReadFull(r, b[:]); err != nil {
				errc <- err
				return
			}
			buttons := b[0]
			x := int(binary.BigEndian.Uint16(b[1:]))
			y := int(binary.BigEndian.Uint16(b[3:]))
			sink.PointerEvent(x, y, buttons)
		case msgClientCutText:
			var b [7]byte
			if _, err := io.ReadFull(r, b[:]); err != nil {
				errc <- err
				return
			}
			n := int(binary.BigEndian.Uint32(b[3:]))
			if n < 0 || n > 1<<20 { // sanity cap (1 MiB)
				errc <- fmt.Errorf("rfb: cut-text length %d out of range", n)
				return
			}
			text := make([]byte, n)
			if _, err := io.ReadFull(r, text); err != nil {
				errc <- err
				return
			}
			// RFB cut text is Latin-1. Forward to the sink, which (for KVM) types
			// it out as synthetic keystrokes.
			sink.CutText(string(text))
		default:
			errc <- fmt.Errorf("rfb: unknown client message type %d", typ)
			return
		}
	}
}

func discard(r *bufio.Reader, n int) (int, error) { return r.Discard(n) }

func writeU32(w *bufio.Writer, v uint32) error {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	_, err := w.Write(b[:])
	return err
}
