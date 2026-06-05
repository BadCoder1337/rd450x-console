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

### Resolution — full POST + BIOS Setup + OS console over SOL (working, 2026-06-05)

> The diagnosis above briefly mis-mapped the SOL port (it is **COM0**, not
> COM1) and wrongly concluded the OS console was unreachable. Corrected below.

**The SOL port is `COM0` — labelled `COM0(SOL)` in the AMI BIOS — which is Linux
`ttyS0` (`0x3F8`).** The BMC SOL is a **real hardware UART bridge** on that port,
*not* a firmware-redirection-only sink. The earlier "0 bytes from the OS" tests
failed because BIOS Console Redirection on COM0 was set to **Always Enable**, so
the BIOS held the UART (via SMM) during OS runtime and blocked the OS↔BMC path.
Releasing the port after the boot loader fixes it.

**Working BIOS config (`COM0(SOL)`):**
Console Redirection **Enabled**, **Redirection After BIOS POST = BootLoader**
(redirect through POST + loader, then hand the UART to the OS), 115200 / 8 /
None / 1, Flow Control None, Terminal Type VT-UTF8.

**Working Proxmox config:** `GRUB_CMDLINE_LINUX="console=tty1 console=ttyS0,115200n8"`
(+ `update-grub`) and `serial-getty@ttyS0` enabled.

**Result:** POST, BIOS Setup (rich VT-UTF8 UI), the boot loader, kernel boot
messages, and the `pve login:` prompt are **all** carried over SOL. Connect with
`rd450x-console` and press Enter.

> **Pitfall:** do **not** point the kernel at `ttyS1`/`0x2F8` — that is *not* the
> SOL port, and BIOS `BootLoader` mode disables it after the loader, so a kernel
> `console=ttyS1` **hangs the boot** at "Loading initial ramdisk". Use `ttyS0`.

**Client note (Windows):** rendering a full-screen TUI (BIOS Setup) needs care.
A naive single-threaded loop freezes on big repaints because Python console
writes can hold the GIL. The client therefore renders on a dedicated thread and
writes via `WriteConsoleW` (ctypes releases the GIL), keeps sends non-blocking,
and disables QuickEdit. This Python/GIL friction is the motivation for a planned
clean **Go** rewrite (goroutines + channels map naturally onto receive / render
/ input).

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
