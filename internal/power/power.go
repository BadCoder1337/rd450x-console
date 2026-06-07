// Package power performs chassis power control for the KVM console's toolbar.
//
// It uses standard IPMI 2.0 chassis control over RMCP+ (UDP 623) via
// github.com/bougou/go-ipmi — the same transport SOL already uses — rather than
// the BMC's proprietary IVTP power opcodes. IPMI chassis control is a documented,
// reliable command set and avoids reverse-engineering the AMI power payload.
//
// JViewer's six power-menu entries collapse onto the five distinct IPMI chassis
// control commands: "Power Off" and "Immediate (hard) Shutdown" are the same
// hard power-down in IPMI, so they are exposed as a single Off action.
package power

import (
	"context"
	"fmt"

	ipmi "github.com/bougou/go-ipmi"
)

// Action is a power operation requested from the toolbar.
type Action string

const (
	On    Action = "on"    // power up
	Off   Action = "off"   // hard power down (JViewer "Power Off" / "Immediate Shutdown")
	ACPI  Action = "acpi"  // graceful soft shutdown via ACPI/OS
	Reset Action = "reset" // hard reset
	Cycle Action = "cycle" // power cycle (off, then on)
)

var actionToControl = map[Action]ipmi.ChassisControl{
	On:    ipmi.ChassisControlPowerUp,
	Off:   ipmi.ChassisControlPowerDown,
	ACPI:  ipmi.ChassisControlSoftShutdown,
	Reset: ipmi.ChassisControlHardReset,
	Cycle: ipmi.ChassisControlPowerCycle,
}

// Controller issues power commands to one BMC. It holds no session: commands are
// infrequent and the MegaRAC stack is fragile, so a fresh RMCP+ session is opened
// per call rather than kept open competing with the KVM/SOL paths.
type Controller struct {
	host           string
	port           int
	user, password string
}

// New returns a Controller for the given BMC. port is the IPMI RMCP+ UDP port
// (usually 623), independent of the KVM video port. The password is never logged.
func New(host string, port int, user, password string) *Controller {
	return &Controller{host: host, port: port, user: user, password: password}
}

// session opens and connects a one-shot RMCP+ client. The returned cleanup must
// be called to release the session.
func (c *Controller) session(ctx context.Context) (*ipmi.Client, func(), error) {
	client, err := ipmi.NewClient(c.host, c.port, c.user, c.password)
	if err != nil {
		return nil, nil, err
	}
	// go-ipmi spawns a 30s keepalive goroutine bound to the ctx passed to
	// Connect; our calls are one-shot, so stop it immediately (mirrors the SOL
	// path) to avoid it racing our reads on the UDP socket.
	connCtx, stopKeepalive := context.WithCancel(ctx)
	if err := client.Connect(connCtx); err != nil {
		stopKeepalive()
		return nil, nil, fmt.Errorf("connect %s:%d: %w", c.host, c.port, err)
	}
	stopKeepalive()
	return client, func() { client.Close(ctx) }, nil
}

// Do performs the given power action.
func (c *Controller) Do(ctx context.Context, action Action) error {
	ctrl, ok := actionToControl[action]
	if !ok {
		return fmt.Errorf("unknown power action %q", action)
	}
	client, cleanup, err := c.session(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := client.ChassisControl(ctx, ctrl); err != nil {
		return fmt.Errorf("chassis control %s: %w", action, err)
	}
	return nil
}

// Status reports whether the host is currently powered on.
func (c *Controller) Status(ctx context.Context) (bool, error) {
	client, cleanup, err := c.session(ctx)
	if err != nil {
		return false, err
	}
	defer cleanup()
	st, err := client.GetChassisStatus(ctx)
	if err != nil {
		return false, err
	}
	return st.PowerIsOn, nil
}
