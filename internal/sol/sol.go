// Package sol provides the Serial-over-LAN console mode of the unified binary.
//
// SOL runs over standard IPMI 2.0 RMCP+ (UDP 623) using github.com/bougou/go-ipmi
// for the session and payload transport, with a faithful port of the Python
// client's interactive layer on top: a raw terminal (alternate screen, VT,
// incremental UTF-8), a decoupled render goroutine, and a telnet-style Ctrl-]
// escape menu (quit / serial break / help).
package sol

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	ipmi "github.com/bougou/go-ipmi"

	"rd450x-console/internal/config"
)

const defaultPort = 623 // IPMI RMCP+ (UDP)

// solReadTimeout bounds how long one SOL poll waits for a BMC reply during the
// interactive phase. The BMC answers an empty poll only when it has serial data
// (or an empty ACK mid-burst); when the console is idle it simply stays silent,
// so the read deadline elapses. go-ipmi's defaults (1s timeout, 4 retries) turn
// each such silent idle poll into a multi-second stall that throttles output to
// a fraction of the link rate; a short timeout with no retries keeps an idle
// poll cheap and lets a data burst drain at the BMC's native cadence. The
// handshake/activation still run with the generous defaults.
const solReadTimeout = 200 * time.Millisecond

// RunCommand implements `rd450x-console sol`.
func RunCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sol", flag.ContinueOnError)
	host := fs.String("host", "", "BMC host (default: IPMI_HOST)")
	user := fs.String("user", "", "BMC user (default: IPMI_USER)")
	port := fs.Int("port", config.PortOr(defaultPort), "RMCP+ UDP port")
	escapeStr := fs.String("escape", "Ctrl-]", "escape/attention key, e.g. 'Ctrl-]', '^x', or '0x1d'")
	force := fs.Bool("force", false, "take over a stale SOL session held by another client")
	info := fs.Bool("info", false, "print BMC device info and power state, then exit (no console)")
	debug := fs.Bool("debug", false, "trace the RMCP+/SOL wire exchange (no raw terminal); diagnostic only")
	if err := fs.Parse(args); err != nil {
		return err
	}

	escape, err := parseEscape(*escapeStr)
	if err != nil {
		return err
	}

	creds := config.Load(*host, *user)
	if creds.Host == "" {
		return fmt.Errorf("missing BMC host: pass -host or set IPMI_HOST")
	}
	if creds.User == "" || creds.Password == "" {
		return fmt.Errorf("missing credential(s): set IPMI_USER and IPMI_PASSWORD (copy .env.example to .env)")
	}

	client, err := ipmi.NewClient(creds.Host, *port, creds.User, creds.Password)
	if err != nil {
		return err
	}
	if *debug {
		client = client.WithDebug(true)
	}

	// go-ipmi spawns an internal 30s keepalive goroutine that calls
	// GetCurrentSessionInfo on the same UDP socket. Our SOL polling (≤100ms)
	// already keeps the session alive, and the keepalive's request/response
	// stream races our SOL reads on the single socket — the plain SOL Exchange
	// grabs the first datagram, so once the keepalive fires our SOL read times
	// out ("UDP read timed out"). The keepalive captures the ctx handed to
	// Connect and only stops on Close, so give Connect a child ctx and cancel it
	// the moment we are connected; our own commands run on the parent ctx.
	connCtx, stopKeepalive := context.WithCancel(ctx)
	if err := client.Connect(connCtx); err != nil {
		stopKeepalive()
		return fmt.Errorf("connect to %s:%d: %w", creds.Host, *port, err)
	}
	stopKeepalive()
	defer client.Close(ctx)

	if *info {
		return showInfo(ctx, client, creds.Host, *port)
	}

	fmt.Printf("Connecting to %s:%d as %q (SOL / IPMI 2.0 RMCP+) ...\n", creds.Host, *port, creds.User)

	if _, err := activate(ctx, client, *force); err != nil {
		if !*force && isAlreadyActive(err) {
			return fmt.Errorf("%w\nHint: a stale SOL session is held on the BMC; re-run with --force to take it over", err)
		}
		return fmt.Errorf("activate SOL payload: %w", err)
	}
	defer deactivate(ctx, client)

	if *debug {
		return diagnoseSOL(ctx, client)
	}

	// Switch the client to the SOL polling profile: a short read timeout and no
	// retries so an unanswered idle poll returns promptly instead of stalling the
	// loop (see solReadTimeout). Restore the defaults before the deferred
	// deactivate so that best-effort teardown still gets a generous timeout — the
	// restore is deferred after the deactivate registration above, so it runs
	// first (LIFO).
	defer client.WithTimeout(time.Duration(ipmi.DefaultLanplusTimeoutSec) * time.Second).WithRetry(ipmi.DefaultLanplusRetries)
	client.WithTimeout(solReadTimeout).WithRetry(0)

	console := NewConsole(client, escape)
	label := console.escapeLabel()
	fmt.Printf("Connected. Escape key is %s; press %s ? for commands, %s q to quit.\n", label, label, label)

	if err := console.Run(ctx); err != nil {
		return err
	}
	fmt.Println("Session closed.")
	return nil
}

// diagnoseSOL sends a handful of empty SOL polls non-interactively (no raw
// terminal) so the go-ipmi debug trace shows exactly what goes on the wire and
// whether the BMC answers. Used to debug the "UDP read timed out" symptom.
func diagnoseSOL(ctx context.Context, client *ipmi.Client) error {
	fmt.Println("--- SOL diagnostic: 10 passive polls, dumping any raw inbound bytes ---")
	var localSeq, remoteSeq, pendingAck uint8 = 1, 0, 0
	for i := 0; i < 10; i++ {
		req := &ipmi.SOLPayloadRequest{SOLPayloadPacket: ipmi.SOLPayloadPacket{
			SequenceNumber:         localSeq,
			AckedSequenceNumber:    remoteSeq,
			AcceptedCharacterCount: pendingAck,
		}}
		res, err := client.SOLPayload(ctx, req)
		if err != nil {
			fmt.Printf("poll %d: ERROR %v\n", i, err)
			return err
		}
		localSeq++
		if localSeq > 0x0F {
			localSeq = 1
		}
		remoteSeq = res.SequenceNumber & 0x0F
		pendingAck = uint8(len(res.CharacterData))
		fmt.Printf("poll %d: OK seq=%d ack=%d ctrl=%#02x bytes=%d %q\n",
			i, res.SequenceNumber, res.AckedSequenceNumber, res.ControlByte,
			len(res.CharacterData), string(res.CharacterData))
	}
	return nil
}

// showInfo prints BMC firmware/IPMI version and power state, then returns.
func showInfo(ctx context.Context, client *ipmi.Client, host string, port int) error {
	dev, err := client.GetDeviceID(ctx)
	if err != nil {
		return fmt.Errorf("get device id: %w", err)
	}
	chassis, err := client.GetChassisStatus(ctx)
	if err != nil {
		return fmt.Errorf("get chassis status: %w", err)
	}
	power := "off"
	if chassis.PowerIsOn {
		power = "on"
	}
	fmt.Printf("BMC          : %s:%d\n", host, port)
	fmt.Printf("Firmware     : %s\n", dev.FirmwareVersionStr())
	fmt.Printf("IPMI version : %d.%d\n", dev.MajorFirmwareRevision, dev.MinorFirmwareRevision)
	fmt.Printf("Power state  : %s\n", power)

	// SOL channel config (baud, enabled, ...). A baud mismatch between the BMC
	// SOL bridge and the host UART shows up as garbage ("?????") in every client,
	// so surface it here. Channel 1 is the LAN/SOL channel on this BMC.
	if sol, err := client.GetSOLConfigParams(ctx, 1); err != nil {
		fmt.Printf("SOL config   : (unavailable: %v)\n", err)
	} else {
		fmt.Println("--- SOL config (channel 1) ---")
		fmt.Println(sol.Format())
	}
	return nil
}

// parseEscape accepts "Ctrl-]" / "^]" / a single char / a 0xNN literal.
func parseEscape(value string) (byte, error) {
	v := strings.TrimSpace(value)
	switch {
	case len(v) == 6 && strings.EqualFold(v[:5], "Ctrl-"):
		return v[5]&^0x20 ^ 0x40, nil // upper, then ^0x40 → control code
	case len(v) == 2 && v[0] == '^':
		return v[1]&^0x20 ^ 0x40, nil
	case len(v) >= 2 && strings.EqualFold(v[:2], "0x"):
		n, err := strconv.ParseUint(v[2:], 16, 8)
		if err != nil {
			return 0, fmt.Errorf("cannot parse escape key %q: %w", value, err)
		}
		return byte(n), nil
	case len(v) == 1:
		return v[0], nil
	default:
		return 0, fmt.Errorf("cannot parse escape key %q", value)
	}
}
