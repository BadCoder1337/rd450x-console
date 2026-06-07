// The out-of-band /control WebSocket: connect/reconnect, a JSON send queue, the
// JSON message dispatcher, and the binary virtual-media request/response plane.
//
// All control traffic (power + virtual media) rides this socket, never the RFB
// video socket (/websockify), so a control action can't stall the framebuffer.
//
// Binary virtual-media wire protocol (big-endian), matching internal/webui/control.go:
//   request  (server→browser): [u8 op][u8 dev][u32 reqId][u64 offset][u32 len][data… if write]
//                              op  0 = read, 1 = write
//                              dev 0 = cd,   1 = fd, 2 = hd  (routes to the mounted backing)
//   response (browser→server): [u32 reqId][u8 status][bytes… if read]
//                              status 0 = ok, 1 = error
// reqId is globally unique on the server side, so the response needs no dev byte.

// The browser is a READ-ONLY sector source; host writes are served by the
// server-side path (scripts/vmedia_probe_go -w), so only op 0 (read) is handled.
const OP_READ = 0;

// dev bytes — keep in sync with kindToDev in internal/webui/control.go.
export const DEV = { cd: 0, fd: 1, hd: 2 };

const wsURL = `${location.protocol === "https:" ? "wss:" : "ws:"}//${location.host}/control`;

let ws = null;
let wsReady = false;
const outbox = []; // JSON messages queued until the socket is open

// Backings serving sectors on demand, keyed by dev byte. A read/write request from
// the server carries the dev it targets; we look the backing up here. Each backing
// exposes read(offset,len) → Uint8Array, write(offset,bytes) → void, close() → void.
const backings = new Map();

// JSON message handlers keyed by message type, plus an onOpen callback list.
const jsonHandlers = new Map();
const openHandlers = [];

// onMessage routes a JSON control reply to its registered handler.
export function onMessage(type, fn) {
  jsonHandlers.set(type, fn);
}

// onOpen registers a callback fired whenever the socket (re)connects — used to
// re-query state (e.g. refresh power) after a bridge restart.
export function onOpen(fn) {
  openHandlers.push(fn);
}

// send queues a JSON control message, flushing immediately if the socket is open.
export function send(obj) {
  const s = JSON.stringify(obj);
  if (wsReady) ws.send(s);
  else outbox.push(s);
}

// setBacking registers (or replaces) the backing serving a device's sectors; the
// server addresses it by dev byte in binary requests.
export function setBacking(dev, backing) {
  backings.set(dev, backing);
}

// removeBacking detaches and closes just one device's backing (leaving any other
// parallel mounts alone). Closing commits a File System Access writable.
export async function removeBacking(dev) {
  const b = backings.get(dev);
  if (!b) return;
  backings.delete(dev);
  try {
    await b.close?.();
  } catch {
    /* best effort — the file may already be gone */
  }
}

// closeAllBackings commits/closes every mounted backing (page unload).
export function closeAllBackings() {
  for (const b of backings.values()) b.close?.();
  backings.clear();
}

// connect opens the socket and keeps it open across bridge restarts / page idle.
export function connect() {
  ws = new WebSocket(wsURL);
  ws.binaryType = "arraybuffer";
  ws.onopen = () => {
    wsReady = true;
    while (outbox.length) ws.send(outbox.shift());
    for (const fn of openHandlers) fn();
  };
  ws.onclose = () => {
    wsReady = false;
    setTimeout(connect, 2000); // survive bridge restarts / page idle
  };
  ws.onerror = () => ws?.close();
  ws.onmessage = (ev) => {
    if (typeof ev.data !== "string") {
      onBinaryRequest(ev.data);
      return;
    }
    let m;
    try {
      m = JSON.parse(ev.data);
    } catch {
      return;
    }
    jsonHandlers.get(m.type)?.(m);
  };
}

// onBinaryRequest serves one on-demand sector read/write. The medium is never
// uploaded in full — only the bytes the host touches cross the wire.
async function onBinaryRequest(buf) {
  const dv = new DataView(buf);
  const op = dv.getUint8(0);
  const dev = dv.getUint8(1);
  const reqId = dv.getUint32(2, false);
  // Reconstruct the 64-bit offset from two 32-bit halves (JS has no native u64).
  const offset = dv.getUint32(6, false) * 2 ** 32 + dv.getUint32(10, false);
  const len = dv.getUint32(14, false);
  try {
    const backing = backings.get(dev);
    if (!backing) throw new Error("no media mounted for device " + dev);
    if (op !== OP_READ) throw new Error("unsupported op " + op); // browser is read-only
    const data = await backing.read(offset, len);
    const out = new Uint8Array(5 + data.byteLength);
    const odv = new DataView(out.buffer);
    odv.setUint32(0, reqId, false);
    odv.setUint8(4, 0); // ok
    out.set(data, 5);
    ws.send(out.buffer);
  } catch (e) {
    // File changed/removed mid-mount — signal an I/O error to the server.
    replyStatus(reqId, 1);
    ioErrorHandler?.(e);
  }
}

function replyStatus(reqId, status) {
  const out = new Uint8Array(5);
  const dv = new DataView(out.buffer);
  dv.setUint32(0, reqId, false);
  dv.setUint8(4, status);
  ws.send(out.buffer);
}

// onIOError lets the vmedia UI surface transient sector read/write failures.
let ioErrorHandler = null;
export function onIOError(fn) {
  ioErrorHandler = fn;
}
