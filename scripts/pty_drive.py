#!/usr/bin/env python3
"""Drive the real rd450x-console `sol` binary under a PTY and measure each
colortest-8 burst — to isolate whether the speed degradation + garbage the user
sees on Windows reproduces on Linux (i.e. is it the Go run-loop/transport, or the
Windows terminal/render layer?).

Sends `colortest-8\\r` N times with a wait between (the user's repro), records all
PTY output with timestamps, then reports per-round byte count + active span and
diffs each round's colortest body against round 1 to flag duplication/garbage.

Run inside WSL from the project dir:
  python3 scripts/pty_drive.py [binary] [rounds]
"""
import os
import pty
import re
import select
import sys
import time

BINARY = sys.argv[1] if len(sys.argv) > 1 else "./rd450x_linux.elf"
ROUNDS = int(sys.argv[2]) if len(sys.argv) > 2 else 5
WAIT = 3.0  # seconds to collect output after each command ("ждём")


def main():
    pid, fd = pty.fork()
    if pid == 0:
        # child: become the sol console attached to this PTY slave
        os.execvp(BINARY, [BINARY, "sol", "-force"])
        os._exit(127)

    chunks = []  # (t, bytes)
    t0 = time.monotonic()

    def pump(duration):
        end = time.monotonic() + duration
        while time.monotonic() < end:
            r, _, _ = select.select([fd], [], [], 0.05)
            if fd in r:
                try:
                    data = os.read(fd, 65536)
                except OSError:
                    return False
                if not data:
                    return False
                chunks.append((time.monotonic() - t0, data))
        return True

    # Wait for "Connected" (binary prints it before entering the alt screen).
    deadline = time.monotonic() + 15
    buf = b""
    while time.monotonic() < deadline:
        r, _, _ = select.select([fd], [], [], 0.1)
        if fd in r:
            data = os.read(fd, 65536)
            chunks.append((time.monotonic() - t0, data))
            buf += data
            if b"Connected" in buf:
                break
    pump(1.0)            # let the alt-screen + shell prompt settle
    os.write(fd, b"\r")  # redraw prompt
    pump(0.8)

    rounds = []  # list of (send_t, [(t,bytes)...])
    for i in range(ROUNDS):
        mark = len(chunks)
        send_t = time.monotonic() - t0
        os.write(fd, b"colortest-8\r")
        pump(WAIT)
        rounds.append((send_t, chunks[mark:]))

    # Quit cleanly: Ctrl-] then q
    os.write(fd, b"\x1dq")
    pump(1.0)
    try:
        os.close(fd)
    except OSError:
        pass
    try:
        os.waitpid(pid, 0)
    except OSError:
        pass

    raw = b"".join(d for _, d in chunks)
    open("pty_capture.bin", "wb").write(raw)

    # Per-round metrics + content diff. The colortest body is the bytes between
    # the command echo and the next shell prompt ("root@...:~#"). Strip that so we
    # compare just the deterministic colortest output across rounds.
    prompt = re.compile(rb"\x1b\][^\x07]*\x07|root@[^\r\n]*[#$]\s*$")
    bodies = []
    print(f"=== {ROUNDS} rounds, binary={BINARY} ===")
    for idx, (send_t, rc) in enumerate(rounds, 1):
        if not rc:
            print(f"r{idx}: NO OUTPUT")
            bodies.append(b"")
            continue
        first = rc[0][0]
        last = rc[-1][0]
        nbytes = sum(len(d) for _, d in rc)
        span = last - first
        thr = nbytes / span / 1024 if span > 0 else 0
        body = b"".join(d for _, d in rc)
        bodies.append(body)
        print(f"r{idx}: bytes={nbytes} first={ (first-send_t)*1000:.0f}ms "
              f"span={span*1000:.0f}ms thr={thr:.1f} KiB/s")

    base = bodies[0]
    for idx in range(1, len(bodies)):
        b = bodies[idx]
        if b == base:
            continue
        off = 0
        while off < len(b) and off < len(base) and b[off] == base[off]:
            off += 1
        print(f"  *** r{idx+1} body DIFF vs r1: len {len(b)} vs {len(base)}, "
              f"first diff @ {off}: r1={base[off:off+24]!r} rN={b[off:off+24]!r}")
    print(f"(raw {len(raw)} bytes -> pty_capture.bin)")


if __name__ == "__main__":
    main()
