package sol

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	ipmi "github.com/bougou/go-ipmi"
)

// isReadTimeout reports whether err is a SOL poll that the BMC simply did not
// answer. Unlike a true IPMI request/response command, the BMC stays silent on
// an empty SOL poll when it has no serial data pending, so go-ipmi's read
// deadline elapses. That is the normal idle case — not a transport failure — so
// the loop must keep polling rather than tear the session down. (See sol.go,
// where the SOL phase runs with a short read timeout and zero retries so these
// surface in well under a second instead of after the default 1s x 4 retries.)
func isReadTimeout(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)
}

// DefaultEscape is the attention key: Ctrl-] (ASCII GS, 0x1d) — the same key
// telnet uses, so it is familiar and rarely needed by the remote shell.
const DefaultEscape = 0x1D

// solBreakControl is byte [3] of an outbound SOL payload requesting a serial
// break (15.9, "Generate BREAK"); matches the Python client's 0b10000.
const solBreakControl = 0x10

// Polling cadence. SOL is a request/response protocol: the BMC piggybacks
// serial output on the ACK to a (possibly empty) packet we send, one packet per
// exchange. While data is flowing we re-poll immediately (drainPoll == 0) so a
// multi-packet BIOS repaint drains at round-trip speed instead of one packet per
// fixed interval; we back off to idlePoll once the BMC returns an empty packet so
// an idle console is not a continuous packet storm. (select still services local
// input between drain polls, so fast output never starves typing.)
const (
	idlePoll  = 100 * time.Millisecond
	drainPoll = 0
)

// Console runs an interactive Serial-over-LAN session over an established IPMI
// 2.0 (RMCP+) lanplus session. All go-ipmi I/O happens on the single loop
// goroutine; a separate render goroutine owns terminal writes (so a slow console
// write never stalls polling) and an input goroutine owns blocking reads.
type Console struct {
	client *ipmi.Client
	escape byte

	term terminal

	// Render handoff. The loop goroutine appends serial output to renderBuf
	// (under renderMu) and pokes renderWake; the render goroutine drains it. The
	// buffer is UNBOUNDED on purpose: a bounded channel would let a slow Windows
	// console (WriteConsole into conhost) back-pressure and *block the loop*,
	// which stalls SOL polling, so the BMC stops getting ACKs and retransmits —
	// the stall shows up as output slowdown and the retransmit storm as on-screen
	// garbage. Matching the Python client's unbounded queue, the loop never waits
	// on rendering.
	renderMu   sync.Mutex
	renderBuf  []byte
	renderWake chan struct{}

	// SOL sequence/ack state — only touched on the loop goroutine.
	localSeq   uint8
	remoteSeq  uint8 // sequence number of the last inbound data packet we rendered
	remoteSize int   // its character count, so a retransmit renders only new bytes
	pendingAck uint8 // chars to ACK on the next outbound packet

	runCtx context.Context
	stop   bool
}

// NewConsole builds a Console for an already-connected client.
func NewConsole(client *ipmi.Client, escape byte) *Console {
	return &Console{
		client:     client,
		escape:     escape,
		renderWake: make(chan struct{}, 1),
		localSeq:   1,
	}
}

// pushRender hands serial output to the render goroutine without ever blocking
// the caller (the SOL loop). Bytes are copied into renderBuf under the lock, so
// the caller may reuse the slice immediately.
func (c *Console) pushRender(b []byte) {
	if len(b) == 0 {
		return
	}
	c.renderMu.Lock()
	c.renderBuf = append(c.renderBuf, b...)
	c.renderMu.Unlock()
	select {
	case c.renderWake <- struct{}{}:
	default: // a wake is already pending; the render goroutine will see the data
	}
}

// flushRender writes everything buffered so far in one coalesced terminal write.
func (c *Console) flushRender() {
	c.renderMu.Lock()
	buf := c.renderBuf
	c.renderBuf = nil
	c.renderMu.Unlock()
	if len(buf) > 0 {
		_ = c.term.write(buf)
	}
}

// escapeLabel renders the escape key as e.g. "Ctrl-]".
func (c *Console) escapeLabel() string {
	return fmt.Sprintf("Ctrl-%c", c.escape+0x40)
}

// notice frames a status line on its own CRLF line so it does not corrupt the
// remote screen, and queues it like everything else to keep a single writer.
func (c *Console) notice(msg string) {
	if c.term != nil {
		c.pushRender([]byte("\r\n*** " + msg + "\r\n"))
	} else {
		fmt.Fprintf(os.Stderr, "*** %s\n", msg)
	}
}

func (c *Console) showHelp() {
	esc := c.escapeLabel()
	c.notice(fmt.Sprintf(
		"escape commands: %s q = quit | %s b = break | %s %s = literal | %s ? = help",
		esc, esc, esc, esc, esc,
	))
}

// feed runs the telnet-style escape state machine over a chunk of local input
// and returns the bytes that should actually be sent to the BMC. quit/brk are
// side-effect requests the loop acts on; armed carries across chunk boundaries.
type escapeState struct {
	escape byte
	armed  bool
}

func (e *escapeState) feed(c *Console, data []byte) (pass []byte, quit, brk bool) {
	for _, b := range data {
		if e.armed {
			e.armed = false
			switch lower(b) {
			case 'q', '.':
				c.notice("escape: quit")
				quit = true
			case 'b':
				c.notice("escape: sending serial break")
				brk = true
			case '?':
				c.showHelp()
			default:
				if b == e.escape { // escape pressed twice → one literal byte
					pass = append(pass, e.escape)
				} else {
					c.notice(fmt.Sprintf("escape: unknown command %q (press %s ? for help)", rune(b), c.escapeLabel()))
				}
			}
		} else if b == e.escape {
			e.armed = true
		} else {
			pass = append(pass, b)
		}
	}
	return pass, quit, brk
}

func lower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// exchange sends one SOL payload packet (carrying chars and/or a control byte,
// and acking the last inbound packet) and routes any *new* serial output to the
// render queue. It returns whether the BMC handed back a data packet (vs an
// empty ACK). The poll loop keys its cadence off the *error* instead — any reply
// (data or empty ACK) means the BMC is engaged, so keep draining; only a read
// timeout means idle — but the data/empty distinction is what ingest is tested
// against, so it is surfaced here too.
//
// SOL is reliable: the BMC retransmits a data packet until we ACK it, and
// go-ipmi's Exchange does no SOL-level dedup, so a retransmit (or a slightly
// extended one) reaches us with the same sequence number as a packet we already
// rendered. Rendering it again would duplicate bytes mid-escape-sequence and
// corrupt the screen, so we mirror pyghmi's _got_sol_payload: render only the
// bytes beyond what we already showed for this sequence number, but always
// re-ACK the full length so the BMC stops resending.
func (c *Console) exchange(chars []byte, control uint8) (active bool, err error) {
	// Every outbound packet carries a nonzero, incrementing sequence number —
	// including empty receive polls. This BMC's SOL is request/response: it only
	// returns a datagram in reply to a packet it must answer, and it stays silent
	// on a seq-0 ack-only packet, so a seq-0 poll never pulls queued output. The
	// nonzero seq (which the BMC must ack) is what elicits the reply that carries
	// serial data. We always advertise our ack of the last inbound data packet.
	req := &ipmi.SOLPayloadRequest{SOLPayloadPacket: ipmi.SOLPayloadPacket{
		SequenceNumber:         c.localSeq,
		AckedSequenceNumber:    c.remoteSeq,
		AcceptedCharacterCount: c.pendingAck,
		ControlByte:            control,
		CharacterData:          chars,
	}}
	res, err := c.client.SOLPayload(c.runCtx, req)
	if err != nil {
		return false, err
	}

	c.localSeq++
	if c.localSeq > 0x0F {
		c.localSeq = 1
	}

	active, fresh := c.ingest(res.SequenceNumber&0x0F, res.CharacterData)
	c.pushRender(fresh) // non-blocking; copies fresh, so reuse is safe
	return active, nil
}

// ingest folds one inbound SOL packet into the sequence/ack state and returns
// whether it was a data packet (active) plus the bytes that are new and should
// be rendered. Pure aside from the sequence/ack fields, so it can be unit-tested
// against the retransmit scenarios that produce on-screen garbage. See exchange.
func (c *Console) ingest(newSeq uint8, data []byte) (active bool, render []byte) {
	if newSeq == 0 {
		// A pure ACK/status packet (no inbound serial data): nothing to render,
		// re-ACK, or drain. Our standing ACK of the previous data packet holds.
		return false, nil
	}

	fresh := data
	if newSeq == c.remoteSeq {
		// Retransmit of a packet we already rendered: show only any bytes beyond
		// what we previously displayed for this sequence number (usually none).
		if len(fresh) > c.remoteSize {
			fresh = fresh[c.remoteSize:]
		} else {
			fresh = nil
		}
	} else {
		c.remoteSeq = newSeq
	}
	c.remoteSize = len(data)
	c.pendingAck = uint8(len(data))
	return true, fresh
}

// renderLoop drains the render buffer and writes to the terminal, coalescing
// everything buffered so far into one write. It owns all terminal writes, so a
// slow console render never stalls the loop goroutine. On shutdown it flushes
// whatever is still buffered (e.g. a final "escape: quit" notice) before exiting.
func (c *Console) renderLoop(done <-chan struct{}) {
	for {
		select {
		case <-done:
			c.flushRender()
			return
		case <-c.renderWake:
			c.flushRender()
		}
	}
}

// Run drives the SOL session until the escape-quit command, ctx cancellation, or
// a transport error. The caller is responsible for ActivatePayload/Deactivate.
func (c *Console) Run(ctx context.Context) error {
	c.runCtx = ctx

	t, err := openTerminal()
	if err != nil {
		return fmt.Errorf("raw terminal: %w", err)
	}
	c.term = t

	done := make(chan struct{})
	renderDone := make(chan struct{})
	go func() {
		c.renderLoop(done)
		close(renderDone)
	}()
	// Tear down in order: signal the render goroutine and wait for it to actually
	// stop before clearing/closing the terminal. Otherwise it can run
	// c.term.write after c.term is nil (nil-pointer panic on exit, e.g. when a
	// final "escape: quit" notice is still queued).
	defer func() {
		close(done)
		<-renderDone
		c.term = nil
		_ = t.close()
	}()

	// Blocking stdin reads on their own goroutine; it outlives Run and dies with
	// the process (a console read cannot be cleanly interrupted), matching the
	// Python client's daemon reader.
	inputCh := make(chan []byte, 16)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := t.read(buf)
			if n > 0 {
				inputCh <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()

	esc := &escapeState{escape: c.escape}

	poll := time.NewTimer(idlePoll)
	defer poll.Stop()
	resetPoll := func(d time.Duration) {
		if !poll.Stop() {
			select {
			case <-poll.C:
			default:
			}
		}
		poll.Reset(d)
	}

	// Prime the link: an initial empty packet establishes the ack baseline. An
	// idle BMC may not answer it (read timeout) — that is fine, keep going.
	if _, err := c.exchange(nil, 0); err != nil && !isReadTimeout(err) {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil

		case data := <-inputCh:
			pass, quit, brk := esc.feed(c, data)
			if brk {
				if _, err := c.exchange(nil, solBreakControl); err != nil && !isReadTimeout(err) {
					return err
				}
			}
			if len(pass) > 0 {
				if _, err := c.exchange(pass, 0); err != nil && !isReadTimeout(err) {
					return err
				}
			}
			if quit {
				return nil
			}
			resetPoll(drainPoll) // drain any response to our input promptly

		case <-poll.C:
			_, err := c.exchange(nil, 0)
			switch {
			case err == nil:
				// The BMC answered — with serial data or, between packets of a
				// burst, an empty ACK. Either way it is engaged and may have more,
				// so re-poll immediately to drain at link speed. Backing off here
				// on an empty ACK (it is not a data packet) would insert an idle
				// gap mid-burst and throttle output to a fraction of the link rate.
				resetPoll(drainPoll)
			case isReadTimeout(err):
				// No reply: the BMC stays silent when it has no serial data, so
				// the read deadline elapsed. The console is idle — back off.
				resetPoll(idlePoll)
			default:
				return err
			}
		}
	}
}

// activate runs Activate Payload (SOL) on the client, retrying once after a
// Deactivate when force is set and the BMC reports the payload already active.
func activate(ctx context.Context, client *ipmi.Client, force bool) (*ipmi.ActivatePayloadResponse, error) {
	req := &ipmi.ActivatePayloadRequest{
		PayloadType:     ipmi.PayloadTypeSOL,
		PayloadInstance: 1,
		// Match ipmitool/pyghmi: the SOL payload rides the session's
		// confidentiality + integrity (the RMCP+ cipher suite already negotiated).
		EnableEncryption:     true,
		EnableAuthentication: true,
	}
	res, err := client.ActivatePayload(ctx, req)
	if err == nil {
		return res, nil
	}
	if force && isAlreadyActive(err) {
		_, _ = client.DeactivatePayload(ctx, &ipmi.DeactivatePayloadRequest{
			PayloadType:     ipmi.PayloadTypeSOL,
			PayloadInstance: 1,
		})
		return client.ActivatePayload(ctx, req)
	}
	return nil, err
}

func deactivate(ctx context.Context, client *ipmi.Client) {
	_, _ = client.DeactivatePayload(ctx, &ipmi.DeactivatePayloadRequest{
		PayloadType:     ipmi.PayloadTypeSOL,
		PayloadInstance: 1,
	})
}

// isAlreadyActive reports whether err is the BMC's "payload already active on
// another session" completion code (0x80).
func isAlreadyActive(err error) bool {
	var respErr *ipmi.ResponseError
	return errors.As(err, &respErr) && respErr.CompletionCode() == 0x80
}
