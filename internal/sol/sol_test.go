package sol

import (
	"sync"
	"testing"
	"time"
)

// blockingTerm is a fake terminal whose write() blocks until released, used to
// prove the render path never back-pressures the SOL loop.
type blockingTerm struct {
	release    chan struct{}
	firstWrite chan struct{}
	once       sync.Once
	mu         sync.Mutex
	got        []byte
}

func (t *blockingTerm) read(b []byte) (int, error) { select {} }
func (t *blockingTerm) write(b []byte) error {
	t.once.Do(func() { close(t.firstWrite) })
	<-t.release
	t.mu.Lock()
	t.got = append(t.got, b...)
	t.mu.Unlock()
	return nil
}
func (t *blockingTerm) close() error { return nil }

// TestPushRenderNeverBlocks guards the Windows degradation+garbage fix: a slow
// console write must not stall the loop goroutine. With the old bounded channel,
// the 65th enqueue blocked once the render goroutine was stuck in write (which on
// Windows stalled SOL polling -> BMC retransmits -> garbage). The unbounded
// buffer must absorb arbitrarily many pushes while a write is in flight.
func TestPushRenderNeverBlocks(t *testing.T) {
	bt := &blockingTerm{release: make(chan struct{}), firstWrite: make(chan struct{})}
	c := &Console{renderWake: make(chan struct{}, 1), term: bt}

	done := make(chan struct{})
	renderDone := make(chan struct{})
	go func() { c.renderLoop(done); close(renderDone) }()

	c.pushRender([]byte("first")) // kicks off the first (blocking) write
	<-bt.firstWrite               // render goroutine is now stuck in write

	const n = 5000
	start := time.Now()
	for i := 0; i < n; i++ {
		c.pushRender([]byte("x"))
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("pushRender blocked while a write was stalled: %d pushes took %v", n, d)
	}

	close(bt.release) // let writes drain
	close(done)
	<-renderDone

	bt.mu.Lock()
	defer bt.mu.Unlock()
	if want := len("first") + n; len(bt.got) != want {
		t.Fatalf("rendered %d bytes, want %d (data lost or duplicated)", len(bt.got), want)
	}
}

func TestParseEscape(t *testing.T) {
	cases := []struct {
		in   string
		want byte
		err  bool
	}{
		{"Ctrl-]", 0x1D, false},
		{"ctrl-]", 0x1D, false},
		{"^]", 0x1D, false},
		{"^x", 0x18, false},
		{"^X", 0x18, false},
		{"0x1d", 0x1D, false},
		{"0x03", 0x03, false},
		{"a", 'a', false},
		{"", 0, true},
		{"nonsense", 0, true},
		{"0xzz", 0, true},
	}
	for _, c := range cases {
		got, err := parseEscape(c.in)
		if c.err {
			if err == nil {
				t.Errorf("parseEscape(%q): expected error, got %#x", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseEscape(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseEscape(%q) = %#x, want %#x", c.in, got, c.want)
		}
	}
}

func TestIngestDedupesRetransmits(t *testing.T) {
	c := &Console{}

	// Fresh data packet: rendered in full, becomes the new ack baseline.
	if active, render := c.ingest(3, []byte("\x1b[31mred")); !active || string(render) != "\x1b[31mred" {
		t.Fatalf("fresh packet: active=%v render=%q", active, render)
	}
	if c.remoteSeq != 3 || c.remoteSize != 8 || c.pendingAck != 8 {
		t.Fatalf("state after fresh: seq=%d size=%d ack=%d", c.remoteSeq, c.remoteSize, c.pendingAck)
	}

	// Exact retransmit (same seq, same bytes): re-ACK but render nothing, else the
	// escape sequence would be duplicated and corrupt the screen.
	if active, render := c.ingest(3, []byte("\x1b[31mred")); !active || render != nil {
		t.Fatalf("retransmit: active=%v render=%q (want no render)", active, render)
	}
	if c.pendingAck != 8 {
		t.Fatalf("retransmit must still ack full length, got ack=%d", c.pendingAck)
	}

	// Extended retransmit (same seq, more bytes): only the new tail renders.
	if active, render := c.ingest(3, []byte("\x1b[31mred!!")); !active || string(render) != "!!" {
		t.Fatalf("extended retransmit: active=%v render=%q (want %q)", active, render, "!!")
	}

	// Next sequence number: fresh again, rendered in full.
	if active, render := c.ingest(4, []byte("ok")); !active || string(render) != "ok" {
		t.Fatalf("next seq: active=%v render=%q", active, render)
	}

	// Pure ACK/status packet (seq 0): not active, nothing to render, state intact.
	if active, render := c.ingest(0, nil); active || render != nil {
		t.Fatalf("ack packet: active=%v render=%q", active, render)
	}
	if c.remoteSeq != 4 {
		t.Fatalf("ack packet must not disturb remoteSeq, got %d", c.remoteSeq)
	}
}

func TestEscapeStateMachine(t *testing.T) {
	c := &Console{escape: DefaultEscape} // term == nil → notices go to stderr
	const esc = DefaultEscape

	t.Run("passthrough", func(t *testing.T) {
		e := &escapeState{escape: esc}
		pass, quit, brk := e.feed(c, []byte("ls\r"))
		if string(pass) != "ls\r" || quit || brk {
			t.Fatalf("got pass=%q quit=%v brk=%v", pass, quit, brk)
		}
	})

	t.Run("quit", func(t *testing.T) {
		e := &escapeState{escape: esc}
		pass, quit, _ := e.feed(c, []byte{esc, 'q'})
		if quit != true || len(pass) != 0 {
			t.Fatalf("got pass=%q quit=%v", pass, quit)
		}
	})

	t.Run("break", func(t *testing.T) {
		e := &escapeState{escape: esc}
		_, _, brk := e.feed(c, []byte{esc, 'B'}) // case-insensitive
		if !brk {
			t.Fatal("expected break")
		}
	})

	t.Run("literal-double-escape", func(t *testing.T) {
		e := &escapeState{escape: esc}
		pass, _, _ := e.feed(c, []byte{esc, esc})
		if len(pass) != 1 || pass[0] != esc {
			t.Fatalf("got %v, want one literal escape", pass)
		}
	})

	t.Run("armed-across-chunks", func(t *testing.T) {
		e := &escapeState{escape: esc}
		if pass, _, _ := e.feed(c, []byte{'x', esc}); string(pass) != "x" {
			t.Fatalf("first chunk pass=%q", pass)
		}
		if _, quit, _ := e.feed(c, []byte{'q'}); !quit {
			t.Fatal("escape armed state did not carry across chunks")
		}
	})
}
