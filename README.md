# rd450x-console

A modern, single-binary remote console for a **Lenovo RD450X** server's AMI
MegaRAC BMC, written in Go. It replaces the legacy `JViewer` Java Web Start
client, which needs an ancient JRE (1.7) and runs with JVM security disabled
(`<all-permissions/>`).

Two modes in one binary, no Java, no browser plugins:

| Mode | What it does | Transport |
|------|--------------|-----------|
| `sol` | Interactive **Serial-over-LAN** console in your terminal | Standard IPMI 2.0 RMCP+ (UDP 623) |
| `kvm` | **KVM / video** console in your browser via embedded noVNC | The BMC's proprietary IVTP/ASPEED protocol (TCP 7582, TLS) |

Both are implemented and working end-to-end against a real RD450X (AMI MegaRAC,
firmware 2.36): full POST + BIOS Setup + OS over SOL, and live decoded video
with keyboard/mouse over KVM.

## Why this exists

The RD450X ships with `JViewer.jar` / `JViewer-SOC.jar`, served from the BMC and
launched through `jviewer.jnlp` (Java Web Start). That stack is effectively dead
on modern systems. `rd450x-console` reimplements the two consoles natively:

- **SOL** turned out to need **no reverse engineering** — the BMC exposes standard
  IPMI 2.0 SOL over RMCP+, so the client is built on the maintained
  [`github.com/bougou/go-ipmi`](https://github.com/bougou/go-ipmi) library.
- **KVM** *did* require reverse-engineering the AMI wire format (there is no
  standard equivalent). The IVTP framing and the ASPEED VQ+JPEG+RC4 video codec
  were re-implemented clean-room in Go; see [`docs/kvm-protocol.md`](docs/kvm-protocol.md).

## Install

Requires **Go 1.24+**. The KVM frontend (noVNC) is a git submodule that is
embedded at build time, so clone recursively:

```sh
git clone --recursive <repo-url>
cd rd450x-console
# already cloned non-recursively? fetch the submodule:
git submodule update --init
```

Build the single binary:

```sh
go build -o bin/rd450x-console ./cmd/rd450x-console
```

```powershell
# Windows
go build -o bin\rd450x-console.exe .\cmd\rd450x-console
```

Prebuilt binaries for Linux, macOS and Windows (amd64 + arm64) are attached to
each [GitHub Release](../../releases).

## Credentials

Credentials are read from the environment **at runtime only** — never from the
command line, and never logged. Copy the example file and fill it in:

```sh
cp .env.example .env   # then edit: IPMI_USER, IPMI_PASSWORD, IPMI_HOST
```

```
IPMI_USER=...
IPMI_PASSWORD=...
IPMI_HOST=192.168.1.90
# optional: IPMI_PORT (SOL only; defaults to 623)
```

`.env` is gitignored. The same variables can be exported in the shell instead.

## Usage

### Serial console (`sol`)

```sh
# Health check — device info, power state, SOL channel config; no console:
rd450x-console sol --info

# Open the interactive serial console:
rd450x-console sol

# Take over a stale SOL session held by another client:
rd450x-console sol --force

# Overrides:
rd450x-console sol --host 192.168.1.90 --user albert --escape "Ctrl-]"
```

**In-console escape commands.** The escape (attention) key defaults to `Ctrl-]`
(like telnet). Press it, then a command:

| Keys | Action |
|------|--------|
| `Ctrl-]` `q` (or `.`) | Quit the console |
| `Ctrl-]` `b` | Send a serial **break** |
| `Ctrl-]` `Ctrl-]` | Send a literal `Ctrl-]` byte to the server |
| `Ctrl-]` `?` | Show help |

All other keystrokes — including `Ctrl-C` — are forwarded to the remote server.
Input is decoded by a single cross-platform [tcell](https://github.com/gdamore/tcell)
layer (identical on Windows, macOS and Linux): arrow keys, Home/End, Page Up/Down,
Insert/Delete and F1–F12 are sent as VT/xterm escape sequences, and control keys
(including the attention key) as their raw bytes. The BMC's VT/ANSI output is
rendered through a `vt10x` terminal emulator, so full-screen TUIs like BIOS Setup
display correctly.

### Video console (`kvm`)

```sh
# Serve the embedded noVNC client and open it in the browser:
rd450x-console kvm                                   # → http://127.0.0.1:6080/vnc.html

# Bind to all interfaces and don't auto-open a browser:
rd450x-console kvm --listen 0.0.0.0:6080 --no-browser
```

Flags: `--host --user --port (7582) --tls (true) --listen (127.0.0.1:6080) --no-browser`.
The binary acts as an RFB (VNC) server in the middle: it logs into the BMC web UI
to mint a session token, completes the IVTP handshake, decodes the ASPEED video
stream into RGB frames for noVNC, and forwards browser keyboard/mouse events back
as USB HID reports. Clipboard text can be pasted into the console as keystrokes.

## How SOL is wired on the RD450X

SOL *activation* succeeds out of the box, but carrying a real console requires the
server's BIOS, bootloader and OS to be configured. The summary below is the
working configuration; **[`docs/sol-setup.md`](docs/sol-setup.md)** has the full
step-by-step (with the GRUB serial-menu and baud-rate gotchas).

- **SOL port = `COM0`** (labelled `COM0(SOL)` in the AMI BIOS) = Linux `ttyS0`
  (`0x3F8`). The BMC SOL is a real hardware UART bridge on that port.
- **BIOS** (`COM0(SOL)`): Console Redirection **Enabled**, **Redirection After
  BIOS POST = BootLoader** (redirect through POST + loader, then release the UART
  to the OS), 115200 / 8 / None / 1, Flow Control None, Terminal Type VT-UTF8.
- **Proxmox / Linux:** `GRUB_CMDLINE_LINUX="console=tty1 console=ttyS0,115200n8"`
  (then `update-grub`) and `serial-getty@ttyS0` enabled.

With that, POST, the rich VT-UTF8 BIOS Setup UI, the boot loader, kernel boot
messages, and the `login:` prompt are **all** carried over SOL. Connect with
`rd450x-console sol` and press Enter.

> **Pitfall:** do **not** point the kernel at `ttyS1`/`0x2F8` — that is not the
> SOL port, and BIOS `BootLoader` mode disables it after the loader, so a kernel
> `console=ttyS1` hangs the boot at "Loading initial ramdisk". Use `ttyS0`.

## Reconnaissance findings

Probes against the BMC that confirmed standard SOL is available (so the
proprietary JViewer-SOC path is unnecessary):

| Check | Result |
|-------|--------|
| RMCP presence ping (UDP 623) | **Pong**, IPMI supported (`entities=0x81`) |
| IPMI 2.0 RMCP+ session | **OK** — firmware 2.36, IPMI v2.0 |
| Power state | `on` |
| SOL payload | 15 instances available |
| `Activate Payload` (SOL) | **succeeds** (`activated=true`) |

## Project layout

```
cmd/rd450x-console/   main entry point + mode dispatch (sol | kvm)
internal/config/      runtime .env / env-var credential loading (password never printed)
internal/sol/         SOL session, console event loop, escape handling, raw terminal
                      (tcell + vt10x emulator; decoupled receive/render/input)
internal/kvm/         KVM client: IVTP transport, web-token auth, HID input
  codec/              ASPEED VQ + JPEG (DCT) + RC4 video decoder
internal/rfb/         minimal RFB 3.8 server bridging decoded frames to noVNC
internal/webui/       go:embed noVNC frontend + WebSocket bridge
  novnc/              noVNC v1.5.0 — git submodule (novnc/noVNC)
scripts/bench_sol_go/ SOL throughput benchmark
scripts/bmc_reset_go/ BMC cold-reset helper
```

## Status & roadmap

- [x] **Serial console (SOL)** — working: POST + BIOS Setup + OS console.
- [x] **KVM / video** — working end-to-end: auth, handshake, decoded video,
      keyboard/mouse HID, clipboard paste.

Remaining polish and JViewer-parity items (power control, virtual media, mouse-mode
negotiation, KM encryption, international keyboard layouts, video-fidelity
validation) are tracked in [`TODO.md`](TODO.md).

## License

[MIT](LICENSE) © Anton Musin
