// Local image-file backing: a plain <input type=file> read-only source. It exposes
// the backing shape consumed by control-socket.js:
//   { name, size, read(off,len)→Uint8Array, close() }
// File.slice(off, off+len) reads only that byte range lazily from disk, so a
// multi-GB image is never loaded or uploaded in full — only the sectors the host
// reads cross the wire.

// fileBackingRO wraps a File picked via <input type=file>. Mounts are read-only;
// host writes are served by the server-side path (scripts/vmedia_probe_go -w).
export function fileBackingRO(f) {
  return {
    name: f.name,
    size: f.size,
    async read(off, len) {
      return new Uint8Array(await f.slice(off, off + len).arrayBuffer());
    },
    async close() {},
  };
}
