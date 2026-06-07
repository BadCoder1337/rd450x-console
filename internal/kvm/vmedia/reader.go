package vmedia

import (
	"fmt"
	"os"
)

// Reader is the backing store for a redirected image: random-access byte ranges
// of a fixed-size medium. The data plane answers the BMC's SCSI reads by calling
// ReadAt. Two implementations exist: FileReader (the localhost turbo path, reads
// an *os.File directly) and, later, a browser-backed reader that fetches ranges
// over the /control WebSocket via File.slice.
//
// Reads past End-of-medium must return io.EOF (or a short count); the SCSI layer
// turns that into a proper sense response rather than a transport error.
type Reader interface {
	// ReadAt fills p with len(p) bytes starting at off, like io.ReaderAt.
	ReadAt(p []byte, off int64) (int, error)
	// Size reports the medium size in bytes.
	Size() int64
}

// FileReader serves a local image file directly (no browser round-trip). It is
// the "localhost turbo path" from docs/kvm-vmedia.md and the vehicle for testing
// the data plane against a known fixture such as bin/test.iso.
type FileReader struct {
	f    *os.File
	size int64
}

// OpenFile opens path read-only as a Reader.
func OpenFile(path string) (*FileReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.IsDir() {
		f.Close()
		return nil, fmt.Errorf("vmedia: %s is a directory, not an image", path)
	}
	return &FileReader{f: f, size: st.Size()}, nil
}

func (r *FileReader) ReadAt(p []byte, off int64) (int, error) { return r.f.ReadAt(p, off) }
func (r *FileReader) Size() int64                             { return r.size }
func (r *FileReader) Close() error                            { return r.f.Close() }
