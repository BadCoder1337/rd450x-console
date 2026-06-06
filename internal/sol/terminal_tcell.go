package sol

import (
	"io"
	"sync"

	"github.com/gdamore/tcell/v2"
	"github.com/hinshun/vt10x"
)

// tcellTerminal is the one cross-platform terminal implementation: tcell owns
// the physical screen and all input decoding, while a vt10x emulator turns the
// BMC's VT/ANSI byte stream into a cell grid we blit into tcell.
//
// Goroutine model (matching Console's expectations):
//   - write()/draw() run on Console's single render goroutine.
//   - pollLoop() runs on its own goroutine: it owns screen.PollEvent(), encodes
//     key events to BMC bytes, and repaints on resize. Decoded bytes (and any
//     emulator-generated replies, e.g. to a DA/DSR query) are delivered to
//     read() over inbytes.
//
// draw() is taken by both goroutines (render on new output, pollLoop on resize),
// so it is serialized with drawMu. emu has its own internal lock, taken via
// Lock/Unlock while we read its cells.
type tcellTerminal struct {
	screen tcell.Screen
	emu    vt10x.Terminal

	inbytes chan []byte // input + emulator replies, drained by read()

	drawMu   sync.Mutex
	leftover []byte // incomplete trailing UTF-8 held between writes (render goroutine only)
}

// vt10x glyph attribute bits. These mirror the unexported iota block in vt10x's
// state.go (attrReverse, attrUnderline, attrBold, attrGfx, attrItalic, attrBlink,
// attrWrap); vt10x is unmaintained so the values are stable. Reverse-video and
// bold-brightening are already folded into the stored FG/BG by vt10x, so we only
// translate the remaining display attributes here.
const (
	vtAttrUnderline = 1 << 1
	vtAttrBold      = 1 << 2
	vtAttrItalic    = 1 << 4
	vtAttrBlink     = 1 << 5
)

// emuResponder feeds the emulator's outbound replies (answerback, cursor/device
// reports) back toward the BMC by funnelling them into the same input channel a
// keystroke would take. A real terminal sends these to the host; dropping them
// can hang a BIOS that waits on a Device Attributes reply.
type emuResponder struct{ ch chan []byte }

func (e emuResponder) Write(p []byte) (int, error) {
	// Non-blocking: emulator replies (cursor/device reports) are ancillary, so if
	// inbytes is momentarily full, drop this reply rather than block the render
	// goroutine — a stalled render stalls SOL polling and triggers retransmits.
	select {
	case e.ch <- append([]byte(nil), p...):
	default:
	}
	return len(p), nil
}

func openTerminal() (terminal, error) {
	screen, err := tcell.NewScreen()
	if err != nil {
		return nil, err
	}
	if err := screen.Init(); err != nil {
		return nil, err
	}
	screen.SetStyle(tcell.StyleDefault)
	screen.Clear()
	// Deliberately no EnableMouse: tcell then neither requests mouse tracking nor
	// reports motion, so mouse movement can never reach the BMC.

	w, h := screen.Size()
	if w <= 0 || h <= 0 {
		w, h = 80, 25
	}

	t := &tcellTerminal{
		screen:  screen,
		inbytes: make(chan []byte, 256),
	}
	t.emu = vt10x.New(vt10x.WithSize(w, h), vt10x.WithWriter(emuResponder{t.inbytes}))

	go t.pollLoop()
	return t, nil
}

// pollLoop owns the tcell event stream. It exits when Fini() makes PollEvent
// return nil, closing inbytes so read() reports EOF and Console's input
// goroutine unwinds.
func (t *tcellTerminal) pollLoop() {
	defer close(t.inbytes)
	for {
		ev := t.screen.PollEvent()
		switch ev := ev.(type) {
		case nil:
			return // screen finalized
		case *tcell.EventKey:
			if b := encodeKey(ev); len(b) > 0 {
				t.inbytes <- b
			}
		case *tcell.EventResize:
			t.screen.Sync()
			w, h := t.screen.Size()
			if w > 0 && h > 0 {
				t.emu.Resize(w, h)
			}
			t.draw()
		}
	}
}

func (t *tcellTerminal) read(buf []byte) (int, error) {
	b, ok := <-t.inbytes
	if !ok {
		return 0, io.EOF
	}
	n := copy(buf, b)
	return n, nil
}

// write folds a chunk of BMC output into the emulator and repaints. Only the
// render goroutine calls it, so leftover needs no lock.
func (t *tcellTerminal) write(b []byte) error {
	if len(t.leftover) > 0 {
		b = append(t.leftover, b...)
		t.leftover = nil
	}
	head, tail := completeUTF8(b)
	if len(tail) > 0 {
		t.leftover = append([]byte(nil), tail...) // tail aliases caller's buffer
	}
	if len(head) == 0 {
		return nil
	}
	if _, err := t.emu.Write(head); err != nil {
		return err
	}
	t.draw()
	return nil
}

// draw blits the emulator grid into tcell and presents it. Serialized so the
// render goroutine (new output) and pollLoop (resize) never interleave frames.
func (t *tcellTerminal) draw() {
	t.drawMu.Lock()
	defer t.drawMu.Unlock()

	t.emu.Lock()
	cols, rows := t.emu.Size()
	cur := t.emu.Cursor()
	curVis := t.emu.CursorVisible()
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			g := t.emu.Cell(x, y)
			ch := g.Char
			if ch == 0 {
				ch = ' '
			}
			t.screen.SetContent(x, y, ch, nil, glyphStyle(g))
		}
	}
	t.emu.Unlock()

	if curVis {
		t.screen.ShowCursor(cur.X, cur.Y)
	} else {
		t.screen.HideCursor()
	}
	t.screen.Show()
}

func (t *tcellTerminal) close() error {
	t.screen.Fini() // unblocks PollEvent → pollLoop exits → inbytes closed
	return nil
}

// glyphStyle maps a vt10x glyph to a tcell style. Reverse video is already baked
// into FG/BG by the emulator, so it is not reapplied here.
func glyphStyle(g vt10x.Glyph) tcell.Style {
	st := tcell.StyleDefault.
		Foreground(vtColor(g.FG)).
		Background(vtColor(g.BG))
	if g.Mode&vtAttrBold != 0 {
		st = st.Bold(true)
	}
	if g.Mode&vtAttrUnderline != 0 {
		st = st.Underline(true)
	}
	if g.Mode&vtAttrItalic != 0 {
		st = st.Italic(true)
	}
	if g.Mode&vtAttrBlink != 0 {
		st = st.Blink(true)
	}
	return st
}

// vtColor maps a vt10x color to a tcell color: the default colors (>= 1<<24) to
// the terminal default, the 256-color palette directly, and anything else as a
// packed 24-bit RGB value.
func vtColor(c vt10x.Color) tcell.Color {
	switch {
	case c >= vt10x.DefaultFG: // DefaultFG/DefaultBG/DefaultCursor (1<<24 + n)
		return tcell.ColorDefault
	case c < 256:
		return tcell.PaletteColor(int(c))
	default:
		return tcell.NewRGBColor(int32((c>>16)&0xff), int32((c>>8)&0xff), int32(c&0xff))
	}
}
