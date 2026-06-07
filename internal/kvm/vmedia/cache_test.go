package vmedia

import (
	"bytes"
	"io"
	"sync"
	"testing"
)

// countingRW is a deterministic in-memory backing that records how many ReadAt
// calls reach it, so a test can prove the cache collapses round-trips.
type countingRW struct {
	mu     sync.Mutex
	b      []byte
	reads  int
	writes int
}

func newCountingRW(n int) *countingRW {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return &countingRW{b: b}
}

func (c *countingRW) Size() int64 { return int64(len(c.b)) }

func (c *countingRW) ReadAt(p []byte, off int64) (int, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	if off >= int64(len(c.b)) {
		return 0, io.EOF
	}
	n := copy(p, c.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (c *countingRW) WriteAt(p []byte, off int64) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes++
	return copy(c.b[off:], p), nil
}

// want is the deterministic byte the backing holds at a given absolute offset.
func wantByte(off int64) byte { return byte(off % 251) }

// TestCacheCoalescesReads proves the cache turns many small sequential reads
// inside one aligned window into a single backing fetch.
func TestCacheCoalescesReads(t *testing.T) {
	back := newCountingRW(4 << 20) // 4 MiB
	c := NewCache(back, nil)

	// 64 sequential 8 KiB reads = 512 KiB, exactly one window. Expect ONE backing read.
	const chunk = 8 * 1024
	for i := 0; i < 64; i++ {
		p := make([]byte, chunk)
		off := int64(i * chunk)
		n, err := c.ReadAt(p, off)
		if err != nil {
			t.Fatalf("ReadAt off=%d: %v", off, err)
		}
		if n != chunk {
			t.Fatalf("ReadAt off=%d returned %d, want %d", off, n, chunk)
		}
		if p[0] != wantByte(off) || p[chunk-1] != wantByte(off+chunk-1) {
			t.Fatalf("ReadAt off=%d returned wrong bytes", off)
		}
	}
	if back.reads != 1 {
		t.Errorf("backing reads = %d, want 1 (window should coalesce)", back.reads)
	}
	st := c.Stats()
	if st.Misses != 1 || st.Hits != 63 {
		t.Errorf("stats = %+v, want 1 miss / 63 hits", st)
	}
}

// TestCacheReadSpanningWindows checks a read that straddles a window boundary
// fetches both windows and stitches the bytes correctly.
func TestCacheReadSpanningWindows(t *testing.T) {
	back := newCountingRW(2 << 20)
	c := NewCache(back, nil)

	// Read 256 KiB starting 128 KiB before a 512 KiB window boundary.
	off := int64(WindowSize - 128*1024)
	p := make([]byte, 256*1024)
	n, err := c.ReadAt(p, off)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != len(p) {
		t.Fatalf("read %d, want %d", n, len(p))
	}
	for i := range p {
		if p[i] != wantByte(off+int64(i)) {
			t.Fatalf("byte %d = %d, want %d (stitch error)", i, p[i], wantByte(off+int64(i)))
		}
	}
	if back.reads != 2 {
		t.Errorf("backing reads = %d, want 2 (two windows spanned)", back.reads)
	}
}

// TestCacheReadAtEOF checks reads at/past end-of-medium return the short count and EOF.
func TestCacheReadAtEOF(t *testing.T) {
	back := newCountingRW(WindowSize + 100) // last window is short
	c := NewCache(back, nil)

	// Read straddling EOF: 200 bytes starting 100 before the end.
	off := c.Size() - 100
	p := make([]byte, 200)
	n, err := c.ReadAt(p, off)
	if err != io.EOF {
		t.Errorf("err = %v, want io.EOF", err)
	}
	if n != 100 {
		t.Errorf("n = %d, want 100 (short read to EOF)", n)
	}
	if !bytes.Equal(p[:n], backRange(back, off, 100)) {
		t.Error("EOF read returned wrong bytes")
	}

	// Read entirely past EOF.
	if n, err := c.ReadAt(make([]byte, 16), c.Size()); n != 0 || err != io.EOF {
		t.Errorf("past-EOF read = (%d, %v), want (0, EOF)", n, err)
	}
}

// TestCacheWriteInvalidates checks WriteAt writes through and evicts the touched
// window so a later read sees the new bytes, not a stale cached copy.
func TestCacheWriteInvalidates(t *testing.T) {
	back := newCountingRW(2 << 20)
	c := NewCache(back, back)

	// Warm the first window.
	if _, err := c.ReadAt(make([]byte, 4096), 0); err != nil {
		t.Fatalf("warm read: %v", err)
	}
	if c.Stats().Misses != 1 {
		t.Fatalf("expected 1 miss warming cache, got %+v", c.Stats())
	}

	// Overwrite bytes inside that window.
	newData := bytes.Repeat([]byte{0xAB}, 512)
	if n, err := c.WriteAt(newData, 1024); err != nil || n != len(newData) {
		t.Fatalf("WriteAt = (%d, %v)", n, err)
	}

	// Re-read the overwritten region: must reflect the write (window was invalidated,
	// so this is a fresh backing fetch — a second miss).
	got := make([]byte, 512)
	if _, err := c.ReadAt(got, 1024); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if !bytes.Equal(got, newData) {
		t.Error("read after write returned stale cached bytes")
	}
	if c.Stats().Misses != 2 {
		t.Errorf("misses = %d, want 2 (invalidation forces a refetch)", c.Stats().Misses)
	}
}

// TestCacheReadOnlyWriteRejected checks a cache with no writer rejects writes.
func TestCacheReadOnlyWriteRejected(t *testing.T) {
	c := NewCache(newCountingRW(1024), nil)
	if _, err := c.WriteAt([]byte{1, 2, 3}, 0); err != errCacheReadOnly {
		t.Errorf("WriteAt err = %v, want errCacheReadOnly", err)
	}
}

// TestCacheEviction checks the LRU honours its window cap (memory stays bounded).
func TestCacheEviction(t *testing.T) {
	back := newCountingRW(int(WindowSize) * (defaultMaxWindows + 10))
	c := NewCache(back, nil)

	// Touch one byte in each of maxWindows+5 distinct windows.
	for i := 0; i < defaultMaxWindows+5; i++ {
		off := int64(i) * WindowSize
		if _, err := c.ReadAt(make([]byte, 1), off); err != nil {
			t.Fatalf("read window %d: %v", i, err)
		}
	}
	c.mu.Lock()
	n := len(c.index)
	c.mu.Unlock()
	if n != defaultMaxWindows {
		t.Errorf("cached windows = %d, want cap %d", n, defaultMaxWindows)
	}

	// The oldest window (0) was evicted; re-reading it is a fresh miss.
	before := c.Stats().Misses
	if _, err := c.ReadAt(make([]byte, 1), 0); err != nil {
		t.Fatalf("re-read window 0: %v", err)
	}
	if c.Stats().Misses != before+1 {
		t.Error("re-reading evicted window 0 should miss")
	}
}

// backRange returns the deterministic backing bytes in [off, off+n) for comparison.
func backRange(b *countingRW, off int64, n int) []byte {
	return b.b[off : off+int64(n)]
}
