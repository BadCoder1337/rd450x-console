package webui

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"rd450x-console/internal/kvm/vmedia"
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

// VMediaController attaches a browser-backed virtual-media device to the host.
// The browser supplies the medium's bytes on demand (File.slice / WebUSB) via the
// backing's ReadAt/WriteAt, which ride the same /control WebSocket; the controller
// opens the AMI IUSB redirection (CD 5120 / FD 5122 / HD 5123) and serves the
// BMC's SCSI requests from that backing. A nil controller disables virtual media.
type VMediaController interface {
	// Attach opens a redirection for kind ("cd"|"fd"|"hd") backed by backing (a
	// medium of size bytes; writable enables host writes). The returned Mount runs
	// until its Close is called.
	Attach(ctx context.Context, kind string, backing vmedia.ReadWriter, size int64, writable bool) (Mount, error)
}

// Mount is an active virtual-media redirection; Close detaches it and releases the
// BMC web session opened for it.
type Mount interface {
	Close() error
}

// Binary control-frame opcodes (server → browser requests). See docs/kvm-vmedia.md.
const (
	opRead  = 0 // [op][dev][u32 reqId][u64 off][u32 len]            → response carries len bytes
	opWrite = 1 // [op][dev][u32 reqId][u64 off][u32 len][len bytes] → response carries only status
)

// Device bytes (frame[1]) tell the browser which mounted backing a request targets,
// so cd/fd/hd can be mounted in parallel over one /control socket. The response is
// unchanged — its globally-unique reqId already routes it back on the Go side.
const (
	devCD = 0
	devFD = 1
	devHD = 2
)

// kindToDev maps a vmedia kind ("cd"|"fd"|"hd") to its device byte. Unknown kinds
// default to devHD (the generic disk type), matching the browser's fallback.
func kindToDev(kind string) byte {
	switch kind {
	case "cd":
		return devCD
	case "fd":
		return devFD
	default:
		return devHD
	}
}

// browserOpTimeout bounds a single on-demand read/write round-trip to the browser.
// Disconnects unblock sooner via context cancellation; this only guards a browser
// that stays connected but stops answering.
const browserOpTimeout = 30 * time.Second

// maxControlFrame caps an inbound control frame. Read responses carry up to the
// IUSB 128 KiB max transfer plus the small response header, so the WS read limit
// must be raised well above coder/websocket's 32 KiB default or those frames fail.
const maxControlFrame = 0x20000 + 4096

// ctrlMsg is a JSON control command from the browser. Virtual-media sector data is
// binary and handled separately (see docs/kvm-vmedia.md); this struct covers the
// JSON control plane only.
type ctrlMsg struct {
	Type     string `json:"type"`
	Action   string `json:"action,omitempty"`
	Kind     string `json:"kind,omitempty"` // vmedia: cd|fd|hd
	Name     string `json:"name,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Writable bool   `json:"writable,omitempty"`
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

// vmResp is a decoded binary response from the browser to one of our requests.
type vmResp struct {
	status byte
	data   []byte
}

// controlConn is one /control WebSocket session. A single read loop demultiplexes
// JSON control messages from binary virtual-media responses; all writes are
// serialized through wmu so JSON replies and binary sector requests never interleave
// on the wire. Several mounts (e.g. a CD and an HD) can be active at once, each
// running its own vmedia.Session goroutine that calls into request() concurrently —
// hence the locked pending map and atomic request id.
type controlConn struct {
	ctx    context.Context
	cancel context.CancelFunc
	ws     *websocket.Conn
	power  ControlHandler
	vmedia VMediaController

	wmu sync.Mutex // serializes every write to ws

	nextID  atomic.Uint32
	pmu     sync.Mutex
	pending map[uint32]chan vmResp

	mmu    sync.Mutex
	mounts map[string]Mount // active redirections keyed by kind (cd|fd|hd)
}

// serveControl runs one /control WebSocket until it closes or the parent context
// is cancelled, then tears down any mounts it opened.
func serveControl(parent context.Context, c *websocket.Conn, power ControlHandler, vm VMediaController) {
	ctx, cancel := context.WithCancel(parent)
	cc := &controlConn{
		ctx: ctx, cancel: cancel, ws: c,
		power: power, vmedia: vm,
		pending: map[uint32]chan vmResp{},
		mounts:  map[string]Mount{},
	}
	cc.serve()
}

func (cc *controlConn) serve() {
	// JSON control messages can trigger slow BMC round-trips (a power command, or a
	// vmedia login that opens a redirection). Handle them on a dedicated serialized
	// worker so they never block the read loop from delivering binary sector
	// responses — otherwise a power poll could stall host disk I/O.
	jsonCh := make(chan []byte, 16)
	go cc.jsonWorker(jsonCh)
	defer close(jsonCh) // runs second: lets the worker drain and exit
	defer cc.cleanup()  // runs first: cancels ctx and detaches mounts

	for {
		typ, data, err := cc.ws.Read(cc.ctx)
		if err != nil {
			return // closed / cancelled
		}
		switch typ {
		case websocket.MessageText:
			// Copy: the ws read buffer may be reused once Read is called again.
			select {
			case jsonCh <- append([]byte(nil), data...):
			case <-cc.ctx.Done():
				return
			}
		case websocket.MessageBinary:
			cc.handleBinaryResponse(data)
		}
	}
}

func (cc *controlConn) jsonWorker(ch chan []byte) {
	for data := range ch {
		cc.handleJSON(data)
	}
}

// cleanup cancels in-flight requests and detaches every mount this connection
// opened (releasing the BMC web sessions), so a closed tab or dropped socket never
// leaves a redirection — or a web session — orphaned on the card.
func (cc *controlConn) cleanup() {
	cc.cancel() // unblocks any request() waiting on cc.ctx

	cc.mmu.Lock()
	ms := cc.mounts
	cc.mounts = map[string]Mount{}
	cc.mmu.Unlock()
	for kind, mt := range ms {
		if err := mt.Close(); err != nil {
			log.Printf("control: detach %s on disconnect: %v", kind, err)
		}
	}
}

// ---- JSON control plane ----------------------------------------------------

func (cc *controlConn) handleJSON(data []byte) {
	var m ctrlMsg
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	switch m.Type {
	case "power":
		cc.handlePower(m)
	case "power.status":
		cc.handlePowerStatus()
	case "vmedia.attach":
		cc.handleAttach(m)
	case "vmedia.detach":
		cc.handleDetach(m)
	default:
		cc.reply(&ctrlReply{Type: "error", Error: "unknown control message: " + m.Type})
	}
}

func (cc *controlConn) handlePower(m ctrlMsg) {
	if cc.power == nil {
		cc.reply(&ctrlReply{Type: "power.result", Action: m.Action, Error: "power control unavailable (no BMC credentials)"})
		return
	}
	if err := cc.power.Power(cc.ctx, m.Action); err != nil {
		log.Printf("control: power %s failed: %v", m.Action, err)
		cc.reply(&ctrlReply{Type: "power.result", Action: m.Action, Error: err.Error()})
		return
	}
	log.Printf("control: power %s issued", m.Action)
	cc.reply(&ctrlReply{Type: "power.result", Action: m.Action, OK: true})
}

func (cc *controlConn) handlePowerStatus() {
	if cc.power == nil {
		cc.reply(&ctrlReply{Type: "power.status", Error: "unavailable"})
		return
	}
	on, err := cc.power.PowerStatus(cc.ctx)
	if err != nil {
		cc.reply(&ctrlReply{Type: "power.status", Error: err.Error()})
		return
	}
	cc.reply(&ctrlReply{Type: "power.status", OK: true, On: on})
}

func (cc *controlConn) handleAttach(m ctrlMsg) {
	if cc.vmedia == nil {
		cc.reply(&ctrlReply{Type: "vmedia.status", Kind: m.Kind, State: "error",
			Error: "virtual media unavailable (no BMC credentials)"})
		return
	}
	if m.Size <= 0 {
		cc.reply(&ctrlReply{Type: "vmedia.status", Kind: m.Kind, State: "error", Error: "invalid medium size"})
		return
	}
	backing := newBrowserBacking(cc, kindToDev(m.Kind), m.Size, m.Writable)
	mt, err := cc.vmedia.Attach(cc.ctx, m.Kind, backing, m.Size, m.Writable)
	if err != nil {
		log.Printf("control: vmedia attach %s (%s, %d bytes): %v", m.Kind, m.Name, m.Size, err)
		cc.reply(&ctrlReply{Type: "vmedia.status", Kind: m.Kind, State: "error", Error: err.Error()})
		return
	}
	cc.setMount(m.Kind, mt)
	log.Printf("control: vmedia attached %s %q (%d bytes, writable=%v)", m.Kind, m.Name, m.Size, m.Writable)
	cc.reply(&ctrlReply{Type: "vmedia.status", Kind: m.Kind, State: "mounted"})
}

func (cc *controlConn) handleDetach(m ctrlMsg) {
	cc.mmu.Lock()
	mt := cc.mounts[m.Kind]
	delete(cc.mounts, m.Kind)
	cc.mmu.Unlock()
	if mt != nil {
		if err := mt.Close(); err != nil {
			log.Printf("control: vmedia detach %s: %v", m.Kind, err)
		}
	}
	cc.reply(&ctrlReply{Type: "vmedia.status", Kind: m.Kind, State: "unmounted"})
}

// setMount stores a mount, detaching any previous mount of the same kind first.
func (cc *controlConn) setMount(kind string, mt Mount) {
	cc.mmu.Lock()
	old := cc.mounts[kind]
	cc.mounts[kind] = mt
	cc.mmu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

func (cc *controlConn) reply(r *ctrlReply) {
	if r == nil {
		return
	}
	cc.wmu.Lock()
	defer cc.wmu.Unlock()
	if err := wsjson.Write(cc.ctx, cc.ws, r); err != nil {
		cc.cancel()
	}
}

// ---- binary virtual-media plane -------------------------------------------

// request sends one binary request to the browser and waits for the matching
// response (correlated by id). op is opRead/opWrite; dev selects the target backing
// (devCD/devFD/devHD) so parallel mounts route to the right source in the browser;
// data is the write payload (nil for reads).
func (cc *controlConn) request(op, dev byte, off uint64, length uint32, data []byte) (vmResp, error) {
	id := cc.nextID.Add(1)
	ch := make(chan vmResp, 1)
	cc.pmu.Lock()
	cc.pending[id] = ch
	cc.pmu.Unlock()
	defer func() {
		cc.pmu.Lock()
		delete(cc.pending, id)
		cc.pmu.Unlock()
	}()

	// [u8 op][u8 dev][u32 reqId][u64 offset][u32 len][data…] — big-endian, matching
	// the browser's onBinaryRequest. The dev byte routes the request to one of the
	// parallel backings; reqId routes the response back here.
	frame := make([]byte, 18+len(data))
	frame[0] = op
	frame[1] = dev
	binary.BigEndian.PutUint32(frame[2:6], id)
	binary.BigEndian.PutUint64(frame[6:14], off)
	binary.BigEndian.PutUint32(frame[14:18], length)
	copy(frame[18:], data)

	if err := cc.writeBinary(frame); err != nil {
		return vmResp{}, err
	}

	timer := time.NewTimer(browserOpTimeout)
	defer timer.Stop()
	select {
	case <-cc.ctx.Done():
		return vmResp{}, cc.ctx.Err()
	case <-timer.C:
		return vmResp{}, errBrowserTimeout
	case r := <-ch:
		return r, nil
	}
}

func (cc *controlConn) writeBinary(b []byte) error {
	cc.wmu.Lock()
	defer cc.wmu.Unlock()
	return cc.ws.Write(cc.ctx, websocket.MessageBinary, b)
}

// handleBinaryResponse routes a browser response — [u32 reqId][u8 status][data…] —
// to the request() waiting on that id.
func (cc *controlConn) handleBinaryResponse(b []byte) {
	if len(b) < 5 {
		return
	}
	id := binary.BigEndian.Uint32(b[0:4])
	r := vmResp{status: b[4]}
	if len(b) > 5 {
		r.data = append([]byte(nil), b[5:]...) // copy: the ws read buffer may be reused
	}
	cc.pmu.Lock()
	ch := cc.pending[id]
	cc.pmu.Unlock()
	if ch != nil {
		ch <- r // buffered (cap 1); request() always drains or has timed out/cancelled
	}
}
