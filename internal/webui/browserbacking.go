package webui

import (
	"errors"
	"io"
)

// errBrowserTimeout is returned when the browser does not answer an on-demand
// read/write within browserOpTimeout (it stayed connected but went unresponsive).
var errBrowserTimeout = errors.New("vmedia: browser did not answer in time")

// errBrowserIO is returned when the browser reports a read/write error (status!=0),
// e.g. the picked file changed or a WebUSB transfer failed.
var errBrowserIO = errors.New("vmedia: browser reported an I/O error")

// errReadOnly is returned by WriteAt on a non-writable backing.
var errReadOnly = errors.New("vmedia: medium is read-only")

// browserBacking is a vmedia.ReadWriter whose bytes live in the browser (a picked
// File via File.slice, or a WebUSB mass-storage device). Each ReadAt/WriteAt is one
// request/response round-trip over the /control WebSocket, so a multi-GB image is
// never uploaded — only the sectors the host actually touches cross the wire.
type browserBacking struct {
	cc       *controlConn
	dev      byte // devCD/devFD/devHD — selects this backing in the browser
	size     int64
	writable bool
}

func newBrowserBacking(cc *controlConn, dev byte, size int64, writable bool) *browserBacking {
	return &browserBacking{cc: cc, dev: dev, size: size, writable: writable}
}

func (b *browserBacking) Size() int64 { return b.size }

// ReadAt asks the browser for len(p) bytes at off. Reads past end-of-medium return
// the short count plus io.EOF, which the SCSI layer turns into a proper sense
// response (the data plane zero-fills the rest).
func (b *browserBacking) ReadAt(p []byte, off int64) (int, error) {
	if off >= b.size {
		return 0, io.EOF
	}
	want := len(p)
	if int64(want) > b.size-off {
		want = int(b.size - off)
	}
	resp, err := b.cc.request(opRead, b.dev, uint64(off), uint32(want), nil)
	if err != nil {
		return 0, err
	}
	if resp.status != 0 {
		return 0, errBrowserIO
	}
	n := copy(p, resp.data)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// WriteAt relays len(p) bytes to the browser to write at off (File System Access
// writable, or a WebUSB WRITE(10)).
func (b *browserBacking) WriteAt(p []byte, off int64) (int, error) {
	if !b.writable {
		return 0, errReadOnly
	}
	resp, err := b.cc.request(opWrite, b.dev, uint64(off), uint32(len(p)), p)
	if err != nil {
		return 0, err
	}
	if resp.status != 0 {
		return 0, errBrowserIO
	}
	return len(p), nil
}
