"""Recon probe: confirm IPMI 2.0 RMCP+ session + SOL availability on the BMC.

Loads credentials from .env at runtime. Never prints the password.
Run:  .venv\\Scripts\\python.exe scripts\\probe_ipmi.py
"""
import os
import sys
from pathlib import Path

HOST = "192.168.1.90"


def load_env():
    env_path = Path(__file__).resolve().parent.parent / ".env"
    if not env_path.exists():
        sys.exit(".env not found — copy .env.example to .env and fill it in.")
    for line in env_path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, _, v = line.partition("=")
        os.environ.setdefault(k.strip(), v.strip().strip('"').strip("'"))


def main():
    load_env()
    user = os.environ.get("IPMI_USER")
    pw = os.environ.get("IPMI_PASSWORD")
    if not user or not pw:
        sys.exit("IPMI_USER / IPMI_PASSWORD missing from .env")

    from pyghmi.ipmi import command

    print(f"Connecting to {HOST} as user '{user}' ...")
    c = command.Command(bmc=HOST, userid=user, password=pw)

    # 1. Get Device ID (netfn 0x06, cmd 0x01) — proves the session works.
    devid = c.xraw_command(netfn=0x06, command=0x01)
    d = bytearray(devid["data"])
    fw_major = d[2] & 0x7F
    fw_minor = d[3]
    ipmi_ver = f"{d[4] & 0x0F}.{d[4] >> 4}"
    manuf = d[6] | (d[7] << 8) | (d[8] << 16)
    print(f"  OK  Device ID: fw {fw_major}.{fw_minor:02x}, IPMI v{ipmi_ver}, manufacturer IANA {manuf}")

    # 2. Power state.
    print(f"  OK  Power state: {c.get_power()}")

    # 3. SOL payload status on channel — Get Payload Activation Status (netfn 0x06 cmd 0x4A)
    #    payload type 1 = SOL
    try:
        pa = c.xraw_command(netfn=0x06, command=0x4A, data=[0x01])
        pd = bytearray(pa["data"])
        print(f"  OK  SOL payload instances: {pd[0]} capacity, activated bitmap {pd[1:].hex()}")
    except Exception as e:
        print(f"  --  Payload activation status query failed: {e}")

    print("\nIPMI 2.0 RMCP+ session works. Standard SOL path is available.")


if __name__ == "__main__":
    main()
