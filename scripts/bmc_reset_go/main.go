// One-shot BMC cold reset + wait-for-recovery, over IPMI 2.0 RMCP+.
//
// Issues a Cold Reset to the management controller (NOT the host — the server
// keeps running) to recover a MegaRAC BMC wedged by repeated SOL re-activation,
// then waits, probing sparsely, until it answers GetDeviceID again.
//
// Usage:  go run ./scripts/bmc_reset_go
// Loads .env at runtime; never prints the password.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	ipmi "github.com/bougou/go-ipmi"

	"rd450x-console/internal/config"
)

func connect(ctx context.Context, host string, port int, user, pass string, to time.Duration, retries int) (*ipmi.Client, error) {
	c, err := ipmi.NewClient(host, port, user, pass)
	if err != nil {
		return nil, err
	}
	c = c.WithTimeout(to).WithRetry(retries)
	connCtx, stop := context.WithCancel(ctx)
	if err := c.Connect(connCtx); err != nil {
		stop()
		return nil, err
	}
	stop() // neuter go-ipmi's keepalive goroutine
	return c, nil
}

func main() {
	config.LoadDotEnv(".env")
	creds := config.Load("", "")
	port := config.PortOr(623)
	if creds.Host == "" || creds.User == "" || creds.Password == "" {
		fmt.Fprintln(os.Stderr, "missing IPMI_HOST/IPMI_USER/IPMI_PASSWORD (.env)")
		os.Exit(1)
	}
	ctx := context.Background()

	client, err := connect(ctx, creds.Host, port, creds.User, creds.Password, time.Second, 4)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	fmt.Printf("Connected to %s:%d — issuing BMC Cold Reset ...\n", creds.Host, port)
	// The BMC usually returns the completion code, then resets; a missing reply
	// is also fine (it reset before answering).
	if err := client.ColdReset(ctx); err != nil {
		fmt.Printf("ColdReset returned: %v (often expected — BMC reset before replying)\n", err)
	} else {
		fmt.Println("ColdReset accepted.")
	}
	_ = client.Close(ctx)

	fmt.Println("Waiting for the BMC to go down and come back (probing sparsely) ...")
	start := time.Now()
	// Give it a head start to actually drop before probing, then probe every 15s.
	const firstWait = 30 * time.Second
	const interval = 15 * time.Second
	const maxWait = 4 * time.Minute
	deadline := start.Add(maxWait)

	time.Sleep(firstWait)

	for time.Now().Before(deadline) {
		c, err := connect(ctx, creds.Host, port, creds.User, creds.Password, 1500*time.Millisecond, 1)
		if err == nil {
			dev, derr := c.GetDeviceID(ctx)
			_ = c.Close(ctx)
			if derr == nil {
				fmt.Printf("BMC back after %.0fs — firmware %s\n", time.Since(start).Seconds(), dev.FirmwareVersionStr())
				return
			}
		}
		fmt.Printf("  ... not yet (%.0fs elapsed)\n", time.Since(start).Seconds())
		time.Sleep(interval)
	}
	fmt.Fprintf(os.Stderr, "BMC did not recover within %s\n", maxWait)
	os.Exit(1)
}
