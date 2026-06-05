"""Listen on SOL for N seconds and dump everything received.

Usage: python scripts/sol_capture.py [seconds]
Loads .env at runtime; never prints the password.
"""
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "src"))
from rd450x_console.config import load_config  # noqa: E402

SECONDS = float(sys.argv[1]) if len(sys.argv) > 1 else 12.0


def main():
    cfg = load_config()
    from pyghmi.ipmi import console as ipmiconsole

    captured = bytearray()
    status = []

    def io(data):
        if isinstance(data, dict):
            status.append(data)
            print("STATUS:", data, flush=True)
        else:
            b = bytes(data)
            captured.extend(b)
            print("RX:", repr(b), flush=True)

    print(f"Activating SOL on {cfg.host}:{cfg.port}; listening {SECONDS}s ...", flush=True)
    c = ipmiconsole.Console(
        bmc=cfg.host, userid=cfg.user, password=cfg.password,
        port=cfg.port, iohandler=io, force=False,
    )
    deadline = time.monotonic() + SECONDS
    while time.monotonic() < deadline:
        ipmiconsole.Console.wait_for_rsp(0.2)
        if c.broken:
            break
    c.close()
    print(f"DONE activated={c.activated} broken={c.broken} total_bytes={len(captured)}", flush=True)


if __name__ == "__main__":
    main()
