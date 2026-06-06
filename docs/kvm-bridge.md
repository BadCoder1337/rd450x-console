# KVM bridge — single Go binary with embedded noVNC

The KVM/video console is implemented as one self-contained Go binary that acts as
a **VNC (RFB) server** translating between the browser and the BMC's proprietary
IVTP/ASPEED protocol. noVNC speaks only RFB, so the binary sits in the middle:

```
browser (noVNC)  ──WebSocket / RFB──▶  rd450x-console  ──IVTP/ASPEED TCP 7582──▶  BMC
   render + input          (Raw RGB frames)   (decode frames, inject HID)
```

The same binary also hosts the SOL console mode (`sol`); see the README for its
flags and escape menu.

## Run

```sh
# noVNC is a git submodule — fetch it once after cloning (or clone with --recursive)
git submodule update --init

go build -o bin/rd450x-console ./cmd/rd450x-console

# KVM: serve noVNC and open it in the browser
export IPMI_USER=... IPMI_PASSWORD=...              # or use .env
./bin/rd450x-console kvm                            # → http://127.0.0.1:6080/vnc.html
./bin/rd450x-console kvm --listen 0.0.0.0:6080 --no-browser
```

Flags: `--host --user --port(7582) --tls(true) --listen(127.0.0.1:6080) --no-browser`.
Credentials come from `IPMI_USER`/`IPMI_PASSWORD` (or `.env`); the password is
used only for the BMC web login and is never logged.

## Layout

```
cmd/rd450x-console/      entry point + mode dispatch (kvm | sol)
internal/config/         .env / env credential loading (password never printed)
internal/kvm/
  ivtp.go                IVTP 8-byte header + opcodes
  transport.go           TLS/TCP dial to 7582 (trust-all, TLS≥1.0)
  session.go             BMC web login → token; type=18 validate packet builder
  client.go              connect + handshake + read loop (fragment reassembly) + keepalive
  command.go             `kvm` subcommand wiring; frame source + HID sink plumbing
  hid.go / usbhid.go     USB HID keyboard/mouse reports (keysym → USB usage)
  latesink.go            late-binding RFB sink (input is dropped until the BMC connects)
  codec/                 ASPEED VQ + JPEG (DCT) + RC4 decoder (decode.go, idct.go,
                         huffman.go, vq.go, yuv.go, quant.go, rc4.go, bitreader.go …)
internal/rfb/            minimal RFB 3.8 server over net.Conn (Raw encoding) + test pattern
internal/webui/          go:embed noVNC + /websockify bridge + browser open
internal/webui/novnc/    noVNC v1.5.0 — git submodule (novnc/noVNC), embedded via go:embed
internal/sol/
  sol.go                 `sol` subcommand wiring + flags + --info
  console.go             RMCP+ SOL session loop, seq/ack, escape menu, render goroutine
  terminal.go            terminal abstraction (raw mode, sizing)
  terminal_tcell.go      tcell + vt10x screen/emulator backend
  keyencode.go           key event → VT/ANSI byte encoding
```

## Status

- **Done:** unified binary; BMC web login + session token; TLS transport; IVTP
  framing; handshake (validate 18 → resp 19 → resume 6); video-fragment
  reassembly (`fragNum` first/last bits); keep-alive; ASPEED codec port (VQ +
  JPEG/DCT + RC4, YUV 4:2:0, skip/delta tiles); decoded frames fed into the RFB
  server and rendered by the embedded noVNC; RFB key/pointer events mapped to
  IVTP HID packets (type 1); clipboard paste as keystrokes.
- **SOL:** ported to Go (`github.com/bougou/go-ipmi`) — `rd450x-console sol`
  with the Ctrl-] escape menu, serial break, `--force`/`--info`, and a
  tcell + vt10x terminal. The binary is self-contained.

Remaining JViewer-parity items (power control, virtual media, mouse-mode
negotiation, KM encryption, international layouts, video-fidelity validation) are
in `TODO.md`. The wire format is documented in `docs/kvm-protocol.md`.
