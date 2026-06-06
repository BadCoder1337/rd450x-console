// Throughput benchmark of the Go (go-ipmi) SOL receive path.
//
// Replicates the real client's hot loop (internal/sol/console.go): a single
// owner that drives SOLPayload request/response exchanges, re-polls immediately
// while data flows (drainPoll == 0), and dedupes retransmitted inbound packets
// by sequence number. We send one command to the logged-in shell on COM0 and
// time the resulting output burst, printing the same metrics as bench_sol.py so
// the two are directly comparable.
//
// Usage:  go run ./scripts/bench_sol_go [command]   (default: colortest-8)
// Loads .env at runtime; never prints the password.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	ipmi "github.com/bougou/go-ipmi"

	"rd450x-console/internal/config"
)

const (
	settle  = 1 * time.Second         // discard pre-existing output before timing
	idleGap = 1500 * time.Millisecond // burst is over after this long with no new bytes
	hardCap = 30 * time.Second
	idle    = 100 * time.Millisecond // poll cadence when no data is flowing
)

// solState mirrors Console's sequence/ack/dedup fields and ingest logic.
type solState struct {
	client      *ipmi.Client
	localSeq    uint8
	remoteSeq   uint8
	remoteSize  int
	pendingAck  uint8
	retransmits int    // cumulative inbound retransmits seen (BMC re-sent same seq)
	baseline    []byte // round-1 rendered bytes; later rounds are diffed against it
}

// exchange sends one SOL packet and returns (active, freshBytes): a faithful
// copy of (*Console).exchange + ingest.
func (s *solState) exchange(ctx context.Context, chars []byte) (bool, []byte, error) {
	// Nonzero incrementing seq on every packet (incl. empty polls): this BMC's
	// request/response SOL only replies to a packet it must ack, so seq-0 polls
	// pull nothing. Mirrors the real client's exchange().
	req := &ipmi.SOLPayloadRequest{SOLPayloadPacket: ipmi.SOLPayloadPacket{
		SequenceNumber:         s.localSeq,
		AckedSequenceNumber:    s.remoteSeq,
		AcceptedCharacterCount: s.pendingAck,
		CharacterData:          chars,
	}}
	res, err := s.client.SOLPayload(ctx, req)
	if err != nil {
		return false, nil, err
	}
	s.localSeq++
	if s.localSeq > 0x0F {
		s.localSeq = 1
	}

	newSeq := res.SequenceNumber & 0x0F
	if newSeq == 0 {
		return false, nil, nil
	}
	fresh := res.CharacterData
	retransmit := newSeq == s.remoteSeq
	if retransmit {
		if len(fresh) > s.remoteSize {
			fresh = fresh[s.remoteSize:]
		} else {
			fresh = nil
		}
		s.retransmits++
	} else {
		s.remoteSeq = newSeq
	}
	s.remoteSize = len(res.CharacterData)
	s.pendingAck = uint8(len(res.CharacterData))
	return true, fresh, nil
}

func main() {
	config.LoadDotEnv(".env")
	cmd := "colortest-8"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	// Leading CR is sacrificial: the first byte of a fresh send is sometimes
	// dropped (no keystroke retransmit), and a bare CR just redraws the prompt.
	payload := append([]byte("\r"+cmd), '\r')

	creds := config.Load("", "")
	port := config.PortOr(623)
	if creds.Host == "" || creds.User == "" || creds.Password == "" {
		fmt.Fprintln(os.Stderr, "missing IPMI_HOST/IPMI_USER/IPMI_PASSWORD (.env)")
		os.Exit(1)
	}

	ctx := context.Background()
	client, err := ipmi.NewClient(creds.Host, port, creds.User, creds.Password)
	if err != nil {
		fmt.Fprintln(os.Stderr, "new client:", err)
		os.Exit(1)
	}
	connCtx, stop := context.WithCancel(ctx)
	if err := client.Connect(connCtx); err != nil {
		stop()
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	stop()
	defer client.Close(ctx)

	// A short read timeout + no retries so an unanswered empty poll (the BMC
	// stays silent when it has no serial data) returns promptly instead of
	// burning the default 1s x 4 retries. Override with args[2]=timeoutMs
	// args[3]=retries to A/B against the default 1000/4. Applied AFTER activation
	// so the IPMI handshake/activate keep the generous default timeout.
	toMs, retries := 80, 0
	if len(os.Args) > 2 {
		fmt.Sscan(os.Args[2], &toMs)
	}
	if len(os.Args) > 3 {
		fmt.Sscan(os.Args[3], &retries)
	}
	timedOut := func(err error) bool {
		return errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)
	}

	fmt.Printf("Activating SOL on %s:%d ...\n", creds.Host, port)
	// Force takeover, matching the benchmark's force=True on the Python side.
	if _, err := client.ActivatePayload(ctx, &ipmi.ActivatePayloadRequest{
		PayloadType: ipmi.PayloadTypeSOL, PayloadInstance: 1,
		EnableEncryption: true, EnableAuthentication: true,
	}); err != nil {
		_, _ = client.DeactivatePayload(ctx, &ipmi.DeactivatePayloadRequest{
			PayloadType: ipmi.PayloadTypeSOL, PayloadInstance: 1,
		})
		if _, err := client.ActivatePayload(ctx, &ipmi.ActivatePayloadRequest{
			PayloadType: ipmi.PayloadTypeSOL, PayloadInstance: 1,
			EnableEncryption: true, EnableAuthentication: true,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "activate SOL:", err)
			os.Exit(1)
		}
	}
	defer client.DeactivatePayload(ctx, &ipmi.DeactivatePayloadRequest{
		PayloadType: ipmi.PayloadTypeSOL, PayloadInstance: 1,
	})

	client = client.WithTimeout(time.Duration(toMs) * time.Millisecond).WithRetry(retries)
	fmt.Printf("(SOL read timeout=%dms retries=%d)\n", toMs, retries)

	s := &solState{client: client, localSeq: 1}

	// One long-lived session, repeated bursts (default 5) with a pause between —
	// reproduces the user's "colortest-8, Enter, wait, repeat x5" where state
	// drift used to slow output and then spew garbage. Override count via args[4].
	rounds := 5
	if len(os.Args) > 4 {
		fmt.Sscan(os.Args[4], &rounds)
	}
	for r := 1; r <= rounds; r++ {
		s.burst(ctx, payload, cmd, r, timedOut)
		if r < rounds {
			// "wait" between repeats — but keep idle-polling like the real client
			// does (it never stops the receive loop), so localSeq/ack churn and any
			// BMC-side drift accumulate exactly as in an interactive session.
			idleEnd := time.Now().Add(3 * time.Second)
			for time.Now().Before(idleEnd) {
				_, _, err := s.exchange(ctx, nil)
				_ = timedOut(err)
			}
		}
	}
}

// burst settles, sends the command once, then drains and times the output,
// printing per-round metrics including any inbound retransmits (the BMC re-sending
// a packet it thinks we failed to ack — the leading indicator of state drift).
func (s *solState) burst(ctx context.Context, payload []byte, cmd string, round int, timedOut func(error) bool) {
	reXmitStart := s.retransmits

	// Settle: drain pre-existing shell output, then discard.
	settleEnd := time.Now().Add(settle)
	for time.Now().Before(settleEnd) {
		_, _, err := s.exchange(ctx, nil)
		_ = timedOut(err)
	}

	tSend := time.Now()
	if _, _, err := s.exchange(ctx, payload); err != nil && !timedOut(err) {
		fmt.Fprintln(os.Stderr, "send:", err)
		return
	}

	var total, dataPkts, polls, timeouts int
	var tFirst, tLast, tPrev time.Time
	var minGap, maxGap, sumGap time.Duration
	minGap = time.Hour
	got := make([]byte, 0, 8192)
	deadline := tSend.Add(hardCap)
	for time.Now().Before(deadline) {
		active, fresh, err := s.exchange(ctx, nil)
		polls++
		if err != nil {
			timeouts++
			if !tFirst.IsZero() && time.Since(tLast) > idleGap {
				break
			}
			continue
		}
		if len(fresh) > 0 {
			got = append(got, fresh...)
			now := time.Now()
			if tFirst.IsZero() {
				tFirst = now
			} else {
				gap := now.Sub(tPrev)
				if gap < minGap {
					minGap = gap
				}
				if gap > maxGap {
					maxGap = gap
				}
				sumGap += gap
			}
			tPrev = now
			tLast = now
			total += len(fresh)
			dataPkts++
		}
		if active {
			continue
		}
		if !tFirst.IsZero() && time.Since(tLast) > idleGap {
			break
		}
	}

	reXmit := s.retransmits - reXmitStart
	if tFirst.IsZero() {
		fmt.Printf("[go r%d] NO OUTPUT (polls=%d timeouts=%d retransmits=%d)\n", round, polls, timeouts, reXmit)
		return
	}
	span := tLast.Sub(tFirst)
	if span <= 0 {
		span = time.Microsecond
	}
	avgGap := time.Duration(0)
	if dataPkts > 1 {
		avgGap = sumGap / time.Duration(dataPkts-1)
	}
	// Content check: colortest-8 is deterministic, so every round should render
	// byte-for-byte identical output. A mismatch (or a length change) is the
	// duplicated/corrupted stream the user sees as garbage.
	diff := ""
	if s.baseline == nil {
		s.baseline = got
	} else if !bytes.Equal(got, s.baseline) {
		off := 0
		for off < len(got) && off < len(s.baseline) && got[off] == s.baseline[off] {
			off++
		}
		diff = fmt.Sprintf("  *** DIFF vs r1: len %d vs %d, first at byte %d", len(got), len(s.baseline), off)
	}
	fmt.Printf("[go r%d] %q bytes=%d span=%dms thr=%.1f KiB/s  pkts=%d polls=%d to=%d reXmit=%d  gap avg=%.1fms max=%.1fms%s\n",
		round, cmd, total, span.Milliseconds(), float64(total)/span.Seconds()/1024,
		dataPkts, polls, timeouts, reXmit,
		float64(avgGap.Microseconds())/1000, float64(maxGap.Microseconds())/1000, diff)
}
