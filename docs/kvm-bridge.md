# KVM bridge — single Go binary with embedded noVNC

The KVM/video console is implemented as one self-contained Go binary that acts as
a **VNC (RFB) server** translating between the browser and the BMC's proprietary
IVTP/ASPEED protocol. noVNC speaks only RFB, so the binary sits in the middle:

```
browser (noVNC)  ──WebSocket / RFB──▶  rd450x-console  ──IVTP/ASPEED TCP 7582──▶  BMC
   render + input          (Raw RGB frames)   (decode frames, inject HID)
```

The same binary will host the SOL console mode (`sol`) once it is ported to Go;
today `sol` defers to the working Python client.

## Run

```powershell
go build -o bin\rd450x-console.exe ./cmd/rd450x-console

# KVM: serve noVNC and open it in the browser
$env:IPMI_USER='...'; $env:IPMI_PASSWORD='...'   # or use .env
.\bin\rd450x-console.exe kvm                       # → http://127.0.0.1:6080/vnc.html
.\bin\rd450x-console.exe kvm --listen 0.0.0.0:6080 --no-browser
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
  codec/decoder.go       ASPEED VQ+JPEG+RC4 decoder  ← STUB (the remaining work)
  command.go             `kvm` subcommand wiring
internal/rfb/            minimal RFB 3.8 server over net.Conn (Raw encoding) + test pattern
internal/webui/          go:embed noVNC + /websockify bridge + browser open
internal/webui/novnc/    vendored noVNC v1.5.0 (embedded into the binary)
internal/sol/            SOL mode (stub → Python client for now)
```

## Status

- **Done:** unified binary; web login + token; TLS transport; IVTP framing;
  handshake (validate 18 → resp 19 → resume 6); video-fragment reassembly
  (`fragNum` first/last bits); keep-alive; minimal RFB server; embedded noVNC
  served end-to-end; animated test pattern proves the noVNC↔RFB pipe.
- **Next (the hard 80%):** port `Decoder.java` → `internal/kvm/codec` (VQ + JPEG
  + RC4, YUV420, skip tiles), then feed decoded frames into an `rfb.Source` to
  replace the test pattern, and map RFB key/pointer events → IVTP HID packets
  (type 1) via an `rfb.Sink`.
- **Then:** port SOL to Go (`github.com/bougou/go-ipmi`) to retire the Python
  client and make the binary fully self-contained.

See `docs/kvm-protocol.md` for the wire format the codec must implement.
