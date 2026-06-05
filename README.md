# rd450x-console

A modern **Serial-over-LAN (SOL) console** for a Lenovo RD450X server, replacing
the legacy `JViewer-SOC.jar` Java Web Start client (which needs an ancient JRE
1.7 and `<all-permissions/>`).

It speaks **standard IPMI 2.0 RMCP+ SOL** over UDP 623 — not the proprietary
AMI JViewer wire protocol on TCP 7582. The reconnaissance below confirmed the
RD450X's AMI MegaRAC BMC exposes standard SOL, so the proprietary path is
unnecessary.

## Reconnaissance findings

Probes against the BMC at `192.168.1.90` (see `scripts/`):

| Check | Result |
|-------|--------|
| RMCP presence ping (UDP 623) | **Pong**, IPMI supported (`entities=0x81`) |
| IPMI 2.0 RMCP+ session | **OK** — firmware 2.36, IPMI v2.0 |
| Power state | `on` |
| SOL payload | 15 instances available |
| `Activate Payload` (SOL) | **succeeds** (`activated=True`) |

**Conclusion:** standard SOL works. No need to download/decompile
`JViewer-SOC.jar`. The client is built on [`pyghmi`](https://opendev.org/x/pyghmi)
(maintained, modern IPMI library) for the RMCP+ transport, wrapped in a clean,
single-threaded interactive console with cross-platform raw-terminal handling.

> **Why the console is silent (diagnosed from the OS side via SSH):** SOL
> *activation* succeeds, but no serial output ever appears. Root cause is
> **server-side**, proven on the running Proxmox host (`pve`):
>
> - The BMC SOL channel is **enabled** (`ipmitool sol info 1` → `Enabled: true`,
>   115.2 kbps).
> - The host exposes exactly **one** live UART: `ttyS0 @ 0x3F8` (COM1, an
>   external DB-9). Ports `0x2F8/0x3E8/0x2E8` are `uart:unknown` (disabled in
>   BIOS). See `/proc/tty/driver/serial`.
> - Writing test bytes to `/dev/ttyS0` increments its TX counter but **arrives
>   as 0 bytes over SOL** → the BMC SOL bridge is **not** wired to the live COM1.
> - The OS has **no serial console** anyway: kernel cmdline is `ro quiet` (no
>   `console=ttyS*`) and `serial-getty@ttyS*` is `inactive`/`disabled`.
>
> **To make SOL actually carry a console, two layers must be fixed:**
>
> 1. **BIOS (firmware — needs physical or KVM access, i.e. project phase 2):**
>    enable **Console Redirection** and the SOL serial port (AMI/Lenovo BIOS →
>    *Console Redirection / Serial Port Sharing*), 115200 8N1, VT100+/VT-UTF8.
>    Note which COM it targets.
> 2. **Proxmox (OS — over SSH):** add `console=tty1 console=ttyS<N>,115200` to
>    the kernel cmdline (PVE 9: edit `/etc/kernel/cmdline` →
>    `proxmox-boot-tool refresh`, or GRUB → `update-grub`) and
>    `systemctl enable --now serial-getty@ttyS<N>.service`, where `ttyS<N>` is
>    the port the BIOS redirects to.
>
> Until the BIOS layer is configured, no OS-side change helps — the SOL UART is
> not even present to the OS. This is the practical reason the project's KVM
> phase (BIOS access) may be needed to finish wiring up the serial console.

### Status: console brought up (2026-06-05)

Both layers are now configured on `pve`, so SOL carries a real console:

1. **BIOS:** Console Redirection enabled on **COM1** (= `0x2F8` = Linux
   `ttyS1`), 115200 / 8N1 / Flow Control None / VT-UTF8 / *Redirection After
   POST = Always Enable*. Verified live — the Aptio Setup screen renders over
   SOL. (The external DB-9 is COM0 = `0x3F8` = `ttyS0` and is **not** the SOL.)
2. **Proxmox OS (GRUB host):**
   - `serial-getty@ttyS1.service` enabled → **login prompt over SOL now**.
   - `/etc/default/grub`: `GRUB_CMDLINE_LINUX="console=tty1 console=ttyS1,115200n8"`,
     `GRUB_TERMINAL="console serial"`,
     `GRUB_SERIAL_COMMAND="serial --unit=1 --speed=115200 ..."` → `update-grub`.
     After the next reboot the GRUB menu and kernel boot messages also appear
     over SOL. (`update-grub` also added a *UEFI Firmware Settings* menu entry,
     so BIOS Setup becomes reachable from GRUB over serial.)

Connect with `rd450x-console` and press Enter to reach `pve login:`.

## Setup

Requires Python 3.9+.

```powershell
python -m venv .venv
.\.venv\Scripts\python.exe -m pip install -e .
Copy-Item .env.example .env   # then edit .env with real credentials
```

Credentials are read from `.env` (gitignored) **at runtime only**:

```
IPMI_USER=...
IPMI_PASSWORD=...
# optional: IPMI_HOST, IPMI_PORT
```

The password is taken exclusively from the `IPMI_PASSWORD` environment variable
and is never passed on the command line or logged.

## Usage

```powershell
# Quick health check — device info + power state, no console:
rd450x-console --info

# Open the interactive serial console:
rd450x-console

# Overrides:
rd450x-console --host 192.168.1.90 --user albert --escape "Ctrl-]"
```

Also runnable as a module: `python -m rd450x_console`.

### In-console escape commands

The **escape (attention) key** defaults to `Ctrl-]` (like telnet). Press it,
then a command:

| Keys | Action |
|------|--------|
| `Ctrl-]` `q` (or `.`) | Quit the console |
| `Ctrl-]` `b` | Send a serial **break** |
| `Ctrl-]` `Ctrl-]` | Send a literal `Ctrl-]` byte to the server |
| `Ctrl-]` `?` | Show help |

All other keystrokes — including `Ctrl-C` — are forwarded to the remote server.
Arrow keys, Home/End, Page Up/Down, Insert/Delete and F1–F4 are translated to
ANSI escape sequences on Windows; on POSIX they pass through natively.

## Project layout

```
src/rd450x_console/
  config.py     # runtime .env / env-var credential loading (password never printed)
  terminal.py   # cross-platform raw terminal (Windows msvcrt+VT / POSIX termios)
  sol.py        # SOL session + single-threaded event loop + escape handling
  cli.py        # argparse entry point  (console scripts: rd450x-console / rd450x-sol)
scripts/
  rmcp_ping.py  # dependency-free RMCP presence ping
  probe_ipmi.py # session + device id + power + SOL payload status
  probe_sol.py  # activate SOL, listen passively
  probe_sol_rt.py # activate SOL, send one CR, capture reply
```

## Roadmap

- [x] Serial console (SOL) — **done** (this client)
- [ ] KVM / video client (port of `JViewer.jar`, TCP 7582, TLS-wrapped AMI
      protocol) — deferred per project scope (serial first, KVM second).
