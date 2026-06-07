package webui

import (
	"context"
	"log"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// ControlHandler executes out-of-band console actions (power, …) issued from the
// injected toolbar over the /control WebSocket. It is deliberately separate from
// the RFB video/input path (/websockify) so a control action never stalls
// framebuffer updates. A nil handler means controls are unavailable (e.g. the
// test-pattern mode with no BMC credentials).
type ControlHandler interface {
	// Power performs a power action ("on", "off", "acpi", "reset", "cycle").
	Power(ctx context.Context, action string) error
	// PowerStatus reports whether the host is currently powered on.
	PowerStatus(ctx context.Context) (on bool, err error)
}

// ctrlMsg is a control command from the browser. Virtual-media data frames are
// binary and handled separately (see docs/kvm-vmedia.md); this struct covers the
// JSON control plane only.
type ctrlMsg struct {
	Type   string `json:"type"`
	Action string `json:"action,omitempty"`
	Kind   string `json:"kind,omitempty"` // vmedia: cd|fd|hd
	Name   string `json:"name,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

// ctrlReply is a JSON response to the browser.
type ctrlReply struct {
	Type   string `json:"type"`
	OK     bool   `json:"ok"`
	Action string `json:"action,omitempty"`
	Kind   string `json:"kind,omitempty"`
	On     bool   `json:"on,omitempty"`
	State  string `json:"state,omitempty"`
	Error  string `json:"error,omitempty"`
}

// serveControl runs the control message loop for one /control WebSocket until it
// closes or ctx is cancelled.
func serveControl(ctx context.Context, c *websocket.Conn, h ControlHandler) {
	for {
		var m ctrlMsg
		if err := wsjson.Read(ctx, c, &m); err != nil {
			return // closed / cancelled / non-JSON frame
		}
		reply := dispatchControl(ctx, h, m)
		if reply == nil {
			continue
		}
		if err := wsjson.Write(ctx, c, reply); err != nil {
			return
		}
	}
}

func dispatchControl(ctx context.Context, h ControlHandler, m ctrlMsg) *ctrlReply {
	switch m.Type {
	case "power":
		if h == nil {
			return &ctrlReply{Type: "power.result", Action: m.Action, Error: "power control unavailable (no BMC credentials)"}
		}
		if err := h.Power(ctx, m.Action); err != nil {
			log.Printf("control: power %s failed: %v", m.Action, err)
			return &ctrlReply{Type: "power.result", Action: m.Action, Error: err.Error()}
		}
		log.Printf("control: power %s issued", m.Action)
		return &ctrlReply{Type: "power.result", Action: m.Action, OK: true}

	case "power.status":
		if h == nil {
			return &ctrlReply{Type: "power.status", Error: "unavailable"}
		}
		on, err := h.PowerStatus(ctx)
		if err != nil {
			return &ctrlReply{Type: "power.status", Error: err.Error()}
		}
		return &ctrlReply{Type: "power.status", OK: true, On: on}

	case "vmedia.attach", "vmedia.detach":
		// The AMI IUSB sector-streaming data plane (ports 5120/5122/5123) is not
		// implemented yet; see docs/kvm-vmedia.md and TODO.md. The control message
		// and the browser-side File.slice read responder are already wired, so only
		// the Go backend remains.
		log.Printf("control: %s kind=%s name=%q size=%d (data plane not implemented)", m.Type, m.Kind, m.Name, m.Size)
		return &ctrlReply{Type: "vmedia.status", Kind: m.Kind, State: "error",
			Error: "virtual media data plane not yet implemented"}

	default:
		return &ctrlReply{Type: "error", Error: "unknown control message: " + m.Type}
	}
}
