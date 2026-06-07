package vmedia

import (
	"container/list"
	"errors"
	"io"
	"sync"
)

// errCacheReadOnly is returned by Cache.WriteAt when the cache wraps a read-only
// backing (no WriterAt was supplied). The SCSI layer guards writes on a nil
// writer before reaching here, so this is a defensive backstop.
var errCacheReadOnly = errors.New("vmedia: cache backing is read-only")

// Cache windowing. The BMC requests at most 128 KiB per SCSI read but bootloader
// and ISO9660 access is largely sequential, so we fetch a larger aligned window
// from the backing on a miss and serve subsequent reads inside it from memory —
// collapsing many small round-trips into one. The defaults bound memory at
// WindowSize*maxWindows = 32 MiB, ample for a boot's working set.
//
// WindowSize is exported because a window is fetched from the backing in ONE
// ReadAt — for the browser backing that is a single /control round-trip whose
// response frame is WindowSize bytes, so the webui control-socket read limit
// (maxControlFrame) MUST exceed it. Keep that limit in step with this constant.
const (
	WindowSize        = 512 * 1024 // aligned fetch granularity (> BMC's 128 KiB request)
	defaultMaxWindows = 64         // LRU capacity → 32 MiB cap
)

// winEntry is one cached aligned window: the backing bytes in [start, start+len(data)).
// data may be shorter than windowSize for the final window at end-of-medium.
type winEntry struct {
	start int64
	data  []byte
}

// Cache is a windowed, read-ahead LRU in front of a Reader. It satisfies Reader
// (and ReadWriter when constructed with a WriterAt) so it can be dropped between
// the SCSI emulator and a high-latency backing — notably the browser File.slice
// backing, where each underlying ReadAt is a /control WebSocket round-trip.
//
// ReadAt is served from cached windows; a miss fetches the whole aligned window
// (one backing read) and caches it. WriteAt writes through to the backing and
// invalidates any overlapping cached windows so read-after-write stays coherent.
//
// One Cache instance backs one device redirection, whose Serve loop drives ReadAt/
// WriteAt sequentially; the mutex only guards against that assumption ever changing.
type Cache struct {
	r          Reader
	w          WriterAt // nil ⇒ read-only
	windowSize int64
	maxWindows int
	size       int64

	mu    sync.Mutex
	lru   *list.List              // front = most-recently-used; values are *winEntry
	index map[int64]*list.Element // window start offset → list element

	hits, misses, fetched int64 // stats (guarded by mu)
}

// NewCache wraps r in a windowed LRU read cache. Pass a non-nil w (typically the
// same backing, which is also a WriterAt) to make the cache writable: writes go
// through to w and invalidate cached windows. Pass nil w for a read-only medium.
func NewCache(r Reader, w WriterAt) *Cache {
	return &Cache{
		r:          r,
		w:          w,
		windowSize: WindowSize,
		maxWindows: defaultMaxWindows,
		size:       r.Size(),
		lru:        list.New(),
		index:      make(map[int64]*list.Element),
	}
}

// Size reports the medium size in bytes.
func (c *Cache) Size() int64 { return c.size }

// ReadAt fills p from the cache, fetching any missing aligned windows from the
// backing. Reads past end-of-medium return a short count plus io.EOF, matching
// the backing's contract (the SCSI layer zero-fills the rest).
func (c *Cache) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("vmedia: negative read offset")
	}
	if off >= c.size {
		return 0, io.EOF
	}
	end := off + int64(len(p))
	if end > c.size {
		end = c.size
	}

	total := 0
	for pos := off; pos < end; {
		winStart := pos - (pos % c.windowSize)
		win, err := c.window(winStart)
		if err != nil {
			return total, err
		}
		inWin := int(pos - winStart)
		if inWin >= len(win) {
			break // window short of windowSize (end-of-medium) → stop
		}
		n := copy(p[pos-off:end-off], win[inWin:])
		total += n
		pos += int64(n)
		if n == 0 {
			break
		}
	}
	if total < len(p) {
		return total, io.EOF
	}
	return total, nil
}

// window returns the cached aligned window starting at start, fetching it from the
// backing on a miss. start must be windowSize-aligned and < size.
func (c *Cache) window(start int64) ([]byte, error) {
	c.mu.Lock()
	if el, ok := c.index[start]; ok {
		c.lru.MoveToFront(el)
		c.hits++
		data := el.Value.(*winEntry).data
		c.mu.Unlock()
		return data, nil
	}
	c.misses++
	c.mu.Unlock()

	// Fetch the whole aligned window (clamped at end-of-medium) in one backing read.
	n := c.windowSize
	if start+n > c.size {
		n = c.size - start
	}
	buf := make([]byte, n)
	got, err := c.r.ReadAt(buf, start)
	if err != nil && err != io.EOF {
		return nil, err
	}
	buf = buf[:got]

	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetched += int64(got)
	// A concurrent fetch may have populated this window meanwhile; prefer the
	// existing entry so all readers share one slice.
	if el, ok := c.index[start]; ok {
		c.lru.MoveToFront(el)
		return el.Value.(*winEntry).data, nil
	}
	el := c.lru.PushFront(&winEntry{start: start, data: buf})
	c.index[start] = el
	for len(c.index) > c.maxWindows {
		back := c.lru.Back()
		if back == nil {
			break
		}
		c.lru.Remove(back)
		delete(c.index, back.Value.(*winEntry).start)
	}
	return buf, nil
}

// WriteAt writes through to the backing and invalidates any cached windows the
// write touches, keeping subsequent reads coherent. It errors on a read-only cache.
func (c *Cache) WriteAt(p []byte, off int64) (int, error) {
	if c.w == nil {
		return 0, errCacheReadOnly
	}
	n, err := c.w.WriteAt(p, off)
	c.invalidate(off, int64(len(p)))
	return n, err
}

// invalidate drops every cached window overlapping [off, off+length).
func (c *Cache) invalidate(off, length int64) {
	if length <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	end := off + length
	for w := off - (off % c.windowSize); w < end; w += c.windowSize {
		if el, ok := c.index[w]; ok {
			c.lru.Remove(el)
			delete(c.index, w)
		}
	}
}

// CacheStats is a snapshot of cache effectiveness for logging.
type CacheStats struct {
	Hits, Misses, FetchedBytes int64
}

// Stats returns a snapshot of window hits/misses and bytes fetched from the backing.
func (c *Cache) Stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{Hits: c.hits, Misses: c.misses, FetchedBytes: c.fetched}
}
