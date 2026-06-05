"""Recon: round-trip test. Activate SOL, send a single Enter, capture reply.

A lone carriage return is the safest possible input -- at a login or shell
prompt it just causes a redraw. Confirms the bidirectional SOL path works.
Loads .env at runtime; never prints the password.
"""
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "src"))
from rd450x_console.config import load_config  # noqa: E402


def main():
    cfg = load_config()
    from pyghmi.ipmi import console as ipmiconsole

    captured = bytearray()
    status = []

    def io(data):
        if isinstance(data, dict):
            status.append(data)
        else:
            captured.extend(bytes(data))

    print(f"Activating SOL on {cfg.host}:{cfg.port} ...")
    c = ipmiconsole.Console(
        bmc=cfg.host, userid=cfg.user, password=cfg.password,
        port=cfg.port, iohandler=io, force=False,
    )
    # settle
    for _ in range(5):
        ipmiconsole.Console.wait_for_rsp(0.2)
    print("sending a single CR ...")
    c.send_data(b"\r")
    deadline = time.monotonic() + 4.0
    while time.monotonic() < deadline:
        ipmiconsole.Console.wait_for_rsp(0.2)
        if c.broken:
            break
    c.close()

    print(f"activated={c.activated} broken={c.broken}")
    print(f"status: {status}")
    print(f"received {len(captured)} bytes")
    if captured:
        print("data:", repr(bytes(captured[-300:])))


if __name__ == "__main__":
    main()
