# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code
in this repository.

## Project goal

A modern remote console for a **Lenovo RD450X** server's AMI MegaRAC BMC,
replacing the legacy `JViewer` Java Web Start client (which needs an ancient JRE
1.7 and disabled JVM security, `<all-permissions/>`).

The client is a single `rd450x-console` Go binary with two subcommands:

- **`sol`** — Serial-over-LAN console over standard IPMI 2.0 RMCP+ (UDP 623).
- **`kvm`** — KVM/video console: an RFB (VNC) server bridging the BMC's
  proprietary IVTP/ASPEED protocol (TCP 7582, TLS) to an embedded noVNC frontend.

**Both are implemented and working.** SOL was built first (no reverse engineering
needed — the BMC speaks standard SOL); the KVM path required reverse-engineering
the AMI wire format and re-implementing the ASPEED video codec clean-room. An
earlier Python prototype was removed once the Go port reached parity.

Remaining work (JViewer-parity polish, not core functionality) is tracked in
`TODO.md`.

## Build, test, run

```sh
git submodule update --init                          # noVNC frontend (embedded via go:embed)
go build -o bin/rd450x-console ./cmd/rd450x-console
go test ./...
go vet ./...

./bin/rd450x-console sol --info                       # health check, no console
./bin/rd450x-console sol                              # interactive serial console
./bin/rd450x-console kvm                              # → http://127.0.0.1:6080/vnc.html
```

Requires Go 1.24+. `internal/webui/novnc` is a git submodule and **must** be
checked out before building — `go:embed` fails without it.

## The target system (from `jviewer.jnlp`)

The RD450X uses an **AMI MegaRAC BMC** (vendor "American Megatrends, Inc.",
client name "JViewer"), firmware 2.36, ASPEED AST SoC. Key facts:

- **BMC host:** `192.168.1.90`, web/IPMI UI on port **80**.
- **SOL:** standard IPMI 2.0 SOL over RMCP+ (UDP 623) — the simple path.
- **KVM/console port:** **7582**, `kvmsecure=1` (TLS-wrapped AMI IVTP protocol).
- **Virtual media ports:** CD `5120`, FD `5122`, HD `5123` (not yet implemented).
- Auth is session-based: the web UI mints a session token (`STOKEN`) and a web
  cookie per login. The KVM client logs in to obtain fresh ones — never hardcode
  the stale example tokens from `jviewer.jnlp`.

## Credentials & secret-handling protocol

IPMI web-UI credentials live in `.env` (gitignored), keyed as in `.env.example`:
`IPMI_USER`, `IPMI_PASSWORD`, `IPMI_HOST`.

**Read `.env` only at runtime, never into the agent's context.** When testing or
debugging, load it inside the program (the Go client reads it via
`internal/config`) — do **not** `cat` / `Get-Content` / `echo` the file or print
the password. Reference the variables by name only.

## Architecture

```
cmd/rd450x-console/   entry point + mode dispatch (sol | kvm)
internal/config/      .env / env-var credential loading (password never printed)
internal/sol/         RMCP+ SOL session loop, seq/ack, Ctrl-] escape menu,
                      decoupled receive/render/input, tcell + vt10x terminal
internal/kvm/         IVTP transport (TLS 7582), web-token auth, frame read loop,
                      USB HID keyboard/mouse input
internal/kvm/codec/   ASPEED VQ + JPEG (DCT) + RC4 video decoder (clean-room)
internal/rfb/         minimal RFB 3.8 server (Raw encoding) → noVNC
internal/webui/       go:embed noVNC + /websockify bridge + browser open
scripts/              SOL benchmark, BMC cold-reset helper
```

Key dependency: `github.com/bougou/go-ipmi` (RMCP+ transport for SOL). The KVM
wire format is documented in `docs/kvm-protocol.md`; the bridge design in
`docs/kvm-bridge.md`.

## Reverse-engineering reference (KVM)

The KVM protocol has already been reverse-engineered; `docs/kvm-protocol.md` is
the clean-room spec the codec implements. For further work, the AMI-copyrighted
jars and decompiled sources live under `re/` (gitignored) — download them from
`http://192.168.1.90:80/Java/release/JViewer.jar` (and `JViewer-SOC.jar`) and
decompile with a Java decompiler (jadx / cfr / procyon, via `scoop`). A passive
packet capture of a live JViewer session remains the best ground truth to
validate the decoder frame-for-frame.
