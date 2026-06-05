"""Recon: activate an SOL payload and passively listen briefly.

Confirms the SOL activation handshake succeeds and the receive path works,
without injecting keystrokes into the live server console. Loads .env at
runtime; never prints the password.
"""
import os
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "src"))
from rd450x_console.config import load_config  # noqa: E402

LISTEN_SECONDS = 4.0


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

    print(f"Activating SOL on {cfg.host}:{cfg.port} as {cfg.user!r} ...")
    c = ipmiconsole.Console(
        bmc=cfg.host, userid=cfg.user, password=cfg.password,
        port=cfg.port, iohandler=io, force=False,
    )
    deadline = time.monotonic() + LISTEN_SECONDS
    while time.monotonic() < deadline:
        ipmiconsole.Console.wait_for_rsp(0.2)
        if c.broken:
            break
    c.close()

    print(f"activated={c.activated} broken={c.broken}")
    print(f"status messages: {status}")
    print(f"received {len(captured)} bytes from console")
    if captured:
        preview = bytes(captured[-200:])
        print("tail:", repr(preview))


if __name__ == "__main__":
    main()
