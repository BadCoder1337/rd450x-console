package vmedia

import (
	"fmt"
	"os"
)

// Reader is the backing store for a redirected image: random-access byte ranges
// of a fixed-size medium. The data plane answers the BMC's SCSI reads by calling
// ReadAt. Implementations: FileReader (the localhost turbo path, an *os.File) and,
// for physical-device passthrough, a raw-volume backing (volume_windows.go).
//
// Reads past End-of-medium must return io.EOF (or a short count); the SCSI layer
// turns that into a proper sense response rather than a transport error.
type Reader interface {
	// ReadAt fills p with len(p) bytes starting at off, like io.ReaderAt.
	ReadAt(p []byte, off int64) (int, error)
	// Size reports the medium size in bytes.
	Size() int64
}

// ReadWriter is a writable backing (floppy / HD / USB). A backing that implements
// it lets the data plane honour SCSI WRITE(10)/WRITE(12). CD-ROM backings are
// read-only and need only implement Reader.
type ReadWriter interface {
	Reader
	// WriteAt writes len(p) bytes at off, like io.WriterAt.
	WriteAt(p []byte, off int64) (int, error)
}

// FileReader serves a local image file directly (no browser round-trip). It is
// the "localhost turbo path" from docs/kvm-vmedia.md and the vehicle for testing
// the data plane against a known fixture (bin/test.iso, test-fd.img, …). Opened
// writable, it also satisfies ReadWriter so the host can write back to the image.
type FileReader struct {
	f    *os.File
	size int64
}

// OpenFile opens path read-only as a Reader.
func OpenFile(path string) (*FileReader, error) { return openFile(path, false) }

// OpenFileRW opens path read-write as a ReadWriter (for floppy/HD/USB write tests).
func OpenFileRW(path string) (*FileReader, error) { return openFile(path, true) }

func openFile(path string, writable bool) (*FileReader, error) {
	flag := os.O_RDONLY
	if writable {
		flag = os.O_RDWR
	}
	f, err := os.OpenFile(path, flag, 0)
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

func (r *FileReader) ReadAt(p []byte, off int64) (int, error)  { return r.f.ReadAt(p, off) }
func (r *FileReader) WriteAt(p []byte, off int64) (int, error) { return r.f.WriteAt(p, off) }
func (r *FileReader) Size() int64                              { return r.size }
func (r *FileReader) Close() error                             { return r.f.Close() }
