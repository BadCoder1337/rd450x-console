package kvm

import (
	"sync/atomic"

	"rd450x-console/internal/rfb"
)

// lateSink is an rfb.Sink whose backing sink is published asynchronously once
// the BMC connection is established. Until then, input events are discarded.
// The swap is race-free via an atomic pointer.
type lateSink struct {
	v atomic.Pointer[rfb.Sink]
}

func newLateSink() *lateSink { return &lateSink{} }

// set publishes the real sink. Safe to call from any goroutine.
func (l *lateSink) set(s rfb.Sink) { l.v.Store(&s) }

func (l *lateSink) KeyEvent(keysym uint32, down bool) {
	if p := l.v.Load(); p != nil {
		(*p).KeyEvent(keysym, down)
	}
}

func (l *lateSink) PointerEvent(x, y int, buttons uint8) {
	if p := l.v.Load(); p != nil {
		(*p).PointerEvent(x, y, buttons)
	}
}

func (l *lateSink) CutText(text string) {
	if p := l.v.Load(); p != nil {
		(*p).CutText(text)
	}
}

var _ rfb.Sink = (*lateSink)(nil)
