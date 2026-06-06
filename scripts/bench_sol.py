"""Throughput benchmark of the Python (pyghmi) SOL receive path.

Mirrors the real client's hot loop (rd450x_console/sol.py): a single owner that
pumps ``wait_for_rsp`` and lets pyghmi receive+ACK. We send one command to the
logged-in shell on COM0 and time the resulting output burst.

Reports:
  * bytes       -- total serial bytes received for the burst
  * first-byte  -- latency from send to the first byte (link/scheduling)
  * stream span -- first byte -> last byte (the steady-state draw time)
  * throughput  -- bytes / stream span  (the apples-to-apples number)

Usage:  python scripts/bench_sol.py [command]   (default: colortest-8)
Loads .env at runtime; never prints the password.
"""
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "src"))
from rd450x_console.config import load_config  # noqa: E402

# Leading CR is sacrificial: the first byte of a fresh send is sometimes dropped
# (no keystroke retransmit), and a bare CR just redraws the prompt.
CMD = b"\r" + (sys.argv[1] if len(sys.argv) > 1 else "colortest-8").encode() + b"\r"
SETTLE = 1.0      # discard pre-existing output before timing
IDLE_GAP = 1.5    # burst is over after this long with no new bytes
HARD_CAP = 30.0   # absolute safety bound


def main():
    cfg = load_config()
    from pyghmi.ipmi import console as ipmiconsole

    rx = bytearray()
    status = []
    last_data = [0.0]

    def io(data):
        if isinstance(data, dict):
            status.append(data)
            return
        rx.extend(bytes(data))
        last_data[0] = time.monotonic()

    print(f"Activating SOL on {cfg.host}:{cfg.port} ...", flush=True)
    c = ipmiconsole.Console(
        bmc=cfg.host, userid=cfg.user, password=cfg.password,
        port=cfg.port, iohandler=io, force=True,
    )

    # Settle: drain whatever the shell already had on screen, then discard it.
    settle_end = time.monotonic() + SETTLE
    while time.monotonic() < settle_end:
        ipmiconsole.Console.wait_for_rsp(0.05)
    rx.clear()

    t_send = time.monotonic()
    c.send_data(CMD)

    t_first = None
    deadline = t_send + HARD_CAP
    while time.monotonic() < deadline:
        ipmiconsole.Console.wait_for_rsp(0.05)
        if c.broken:
            break
        if rx and t_first is None:
            t_first = last_data[0]  # set on the first io() callback
        if t_first is not None and (time.monotonic() - last_data[0]) > IDLE_GAP:
            break
    c.close()

    total = len(rx)
    if t_first is None:
        print(f"NO OUTPUT (broken={c.broken} status={status})", flush=True)
        return
    span = max(last_data[0] - t_first, 1e-6)
    print(
        f"[python] cmd={CMD!r} bytes={total} "
        f"first_byte={ (t_first - t_send)*1000:.0f}ms "
        f"stream_span={span*1000:.0f}ms "
        f"throughput={total/span/1024:.1f} KiB/s",
        flush=True,
    )


if __name__ == "__main__":
    main()
