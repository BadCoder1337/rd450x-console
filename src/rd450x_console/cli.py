"""Command-line entry point for the RD450X serial console."""

from __future__ import annotations

import argparse
import sys

from . import __version__
from .config import ConfigError, load_config
from .sol import DEFAULT_ESCAPE, SerialConsole


def _parse_escape(value: str) -> int:
    """Accept 'Ctrl-]' / '^]' / a single char / a 0xNN literal."""
    v = value.strip()
    if v.lower().startswith("ctrl-") and len(v) == 6:
        return ord(v[-1].upper()) ^ 0x40
    if v.startswith("^") and len(v) == 2:
        return ord(v[1].upper()) ^ 0x40
    if v.lower().startswith("0x"):
        return int(v, 16) & 0xFF
    if len(v) == 1:
        return ord(v)
    raise argparse.ArgumentTypeError(f"cannot parse escape key {value!r}")


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="rd450x-console",
        description=(
            "Serial-over-LAN console for the Lenovo RD450X (AMI MegaRAC BMC) "
            "over standard IPMI 2.0 RMCP+. Replaces the legacy JViewer-SOC "
            "Java client. Credentials come from .env (IPMI_USER / "
            "IPMI_PASSWORD); the password is never passed on the command line."
        ),
    )
    p.add_argument("--host", help="BMC hostname/IP (default: IPMI_HOST or 192.168.1.90)")
    p.add_argument("--port", type=int, help="RMCP+ UDP port (default: 623)")
    p.add_argument("--user", help="IPMI user (default: IPMI_USER from env/.env)")
    p.add_argument(
        "--escape",
        type=_parse_escape,
        default=DEFAULT_ESCAPE,
        help="Escape/attention key, e.g. 'Ctrl-]' (default), '^x', or '0x1d'.",
    )
    p.add_argument(
        "--force",
        action="store_true",
        help="Take over a stale SOL session held by another client "
        "(deactivates the existing payload first).",
    )
    p.add_argument(
        "--info",
        action="store_true",
        help="Print BMC device info and power state, then exit (no console).",
    )
    p.add_argument("--version", action="version", version=f"%(prog)s {__version__}")
    return p


def _show_info(config) -> int:
    from pyghmi.ipmi import command

    c = command.Command(bmc=config.host, userid=config.user, password=config.password, port=config.port)
    devid = bytearray(c.xraw_command(netfn=0x06, command=0x01)["data"])
    fw = f"{devid[2] & 0x7F}.{devid[3]:02x}"
    ipmi_ver = f"{devid[4] & 0x0F}.{devid[4] >> 4}"
    print(f"BMC          : {config.host}:{config.port}")
    print(f"Firmware     : {fw}")
    print(f"IPMI version : {ipmi_ver}")
    print(f"Power state  : {c.get_power().get('powerstate')}")
    return 0


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        config = load_config(host=args.host, port=args.port, user=args.user)
    except ConfigError as exc:
        print(str(exc), file=sys.stderr)
        return 2

    try:
        if args.info:
            return _show_info(config)
        return SerialConsole(config, escape=args.escape, force=args.force).run()
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
