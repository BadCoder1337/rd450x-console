# KVM toolbar extension — power control & virtual media

This documents how JViewer-parity **power control** and **virtual media** are added
to the KVM console without forking the embedded noVNC client, and the design of
the browser-driven virtual-media data path.

## Decision: extend stock noVNC, don't replace it

An earlier evaluation (see git history of `TODO.md`) considered bridging to an
existing open KVM frontend (PiKVM/kvmd, NanoKVM, …) for fuller parity. That was
rejected: every mainstream open IP-KVM stack uses a **device-local-image** model
(the image is uploaded to the KVM box and exposed to the host via a Linux USB
gadget). Our AMI MegaRAC BMC uses the opposite, **client-streams-sectors** model
(the BMC presents a USB CD-ROM to the host and pulls sectors from *us* on demand
over ports 5120/5122/5123). No open client speaks that, so the virtual-media data
plane is unavoidably ours regardless of frontend — and adopting another frontend
would also cost us the working RFB/noVNC video path. We therefore keep stock
noVNC and extend it.

## Architecture: two planes

```
                 browser (noVNC + injected toolbar)
                  │                         │
       RFB video/input                control plane
       /websockify (binary WS)        /control (JSON + binary WS)
                  │                         │
            internal/rfb              internal/webui/control.go
                  │                    ┌────┴─────────────┐
            internal/kvm           power               vmedia
          (IVTP/ASPEED 7582)   internal/power      internal/kvm/vmedia
                  │            (IPMI chassis,        (AMI IUSB 5120/5122/5123,
                  ▼             RMCP+ 623)            *not yet implemented*)
                 BMC ◀──────────────┴───────────────────────┘
```

The **control plane is deliberately separate from the RFB video socket** so a
power command or a virtual-media sector read can never stall framebuffer updates.

## Injection (keeps the submodule pristine)

`internal/webui/novnc` (the noVNC v1.5.0 submodule) is **never modified**. At
serve time `webui.go` rewrites only the response for `/vnc.html`, splicing a
stylesheet + script tag in just before `</body>`:

```html
<link rel="stylesheet" href="rd450x/inject.css"><script defer src="rd450x/inject.js"></script>
```

`internal/webui/assets/inject.{js,css}` (a separate `go:embed`) build the toolbar
entries **using noVNC's own classes** (`noVNC_button`, `noVNC_panel`,
`noVNC_open`, `noVNC_selected`, `noVNC_heading`) so they render identically to the
native controls, and insert them next to the settings button (inside
`#noVNC_control_bar .noVNC_scroll`). Because we only inject a `<script>`, the
submodule can be bumped without merge conflicts.

## Power control — implemented

`internal/power` performs power actions over **standard IPMI 2.0 chassis control,
RMCP+ UDP 623** (`github.com/bougou/go-ipmi`, already used by SOL) rather than the
proprietary IVTP power opcodes — it is documented, reliable, and needs no RE. A
fresh one-shot session is opened per action (actions are rare; the MegaRAC stack
is fragile, so no long-lived session competes with KVM/SOL).

JViewer's six power-menu entries map onto the **five distinct** IPMI commands
("Power Off" and "Immediate Shutdown" are the same hard power-down):

| Toolbar button   | `power.Action` | IPMI ChassisControl       |
|------------------|----------------|---------------------------|
| Power On         | `on`           | PowerUp (1)               |
| ACPI Shutdown    | `acpi`         | SoftShutdown (5)          |
| Power Off (hard) | `off`          | PowerDown (0)             |
| Reset            | `reset`        | HardReset (3)             |
| Power Cycle      | `cycle`        | PowerCycle (2)            |

Live power state comes from `GetChassisStatus`. Destructive actions confirm in the
browser first. Control messages (JSON over `/control`):

```jsonc
// browser → server
{ "type": "power", "action": "on|off|acpi|reset|cycle" }
{ "type": "power.status" }
// server → browser
{ "type": "power.result", "action": "...", "ok": true }
{ "type": "power.status", "ok": true, "on": true }
```

## Virtual media — UI + browser flow implemented, data plane pending

The toolbar's Virtual Media panel picks a local `.iso`/`.img` via
`<input type=file>` (portable; the File System Access API is Chrome/Edge-only) and
sends an attach control message. The **efficient on-demand read flow** is the same
one OpenBMC's Redfish "proxy mode" uses (browser acts as the data source):

- `File.slice(offset, offset+len).arrayBuffer()` reads **only that byte range**,
  lazily from disk — confirmed by the W3C File API spec and OpenBMC's `jsnbd`.
  The multi-GB image is **never uploaded or loaded in full**; only the sectors the
  host actually reads cross the wire.
- The browser keeps the `File` snapshot for the whole mount. If the file is
  changed/removed mid-mount, `slice()` reads throw `NotReadableError`/
  `NotFoundError`; the responder reports a read error.

Read wire protocol over `/control` (binary, big-endian), wired in `inject.js` and
**ready for the Go backend**:

```
request  (server → browser): [u32 reqId][u64 offset][u32 len]   (16 bytes)
response (browser → server): [u32 reqId][u8 status][bytes…]      (status 0=ok, 1=error)
```

### Sizing the data plane (from RE)

The AMI IUSB data plane (`internal/kvm/vmedia`, **to be written** — see `TODO.md`)
must answer SCSI reads from the BMC and fetch the bytes via the read protocol
above. From the RE references:

- **Max transfer per BMC request: 128 KiB** (`MAX_READ_SIZE = 0x20000`). Serve
  arbitrary `(offset, len ≤ 128 KiB)`.
- Block size: **CD 2048 B**, **floppy/HD 512 B**. Access is **random** (boot jumps
  around), so the backing read must seek, not stream.
- Optimisation: Go should fetch **larger aligned windows** (e.g. 512 KiB–1 MiB)
  than the BMC's request and keep a small **LRU cache** — bootloader/ISO9660 reads
  are largely sequential, so this collapses many round-trips. (`jsnbd` does none of
  this; it's a clear place to do better.)
- IUSB framing: 32-byte header + payload; header length field is little-endian, the
  SCSI CDB inside is big-endian. Min command set before READ(10): TEST UNIT READY,
  INQUIRY, READ CAPACITY, MODE SENSE; CD adds the MMC probes (READ TOC, GET
  CONFIGURATION, …).

### IUSB wire protocol (reverse-engineered from JViewer)

Decoded from the decompiled `com.ami.iusb.*` sources under `re/` (the SCSI
*emulation* is native — `executeCDROMSCSICmd` in `libjavacdromwrapper` — but the
transport, handshake and framing are pure Java and reproduced below). Implemented
in `internal/kvm/vmedia/iusb.go`.

**Status: working end-to-end for CD-ROM, floppy, and HD/USB.** Verified against the
live RD450X + Proxmox host: our `internal/kvm/vmedia` data plane serves the test
media and the host reads them correctly (read-only):

| `-type` | port | device      | block | emulator         | test image      | host node / blkid                  |
|---------|------|-------------|-------|------------------|-----------------|------------------------------------|
| cd      | 5120 | CD-ROM (MMC)| 2048  | `NewCDROM`       | `bin/test.iso`  | `/dev/cdrom→sr0`, iso9660 RD450X_TEST |
| fd      | 5122 | Direct-Access| 512  | `NewDisk`        | `bin/test-fd.img`| `sd?` (Virtual Floppy0), vfat RD450X_FD |
| hd      | 5123 | Direct-Access| 512  | `NewDisk`        | `bin/test-hd.img`| `sd?` (Virtual HDisk0), vfat RD450X_HD |

Drive it with `scripts/vmedia_probe_go -type cd|fd|hd -iso <file>` (`-jnlp` dumps the
vmedia config). Build the media with `go run ./scripts/mkiso_go` and
`go run ./scripts/mkimg_go`. All media is **read-only** so far (CD is inherently
read-only; floppy/HD write support — WRITE(10) + a writable backing — is future work).

**Transport.** One **plaintext TCP** socket **per device** — the jnlp's
`vmsecure=0` on this BMC means virtual media is *not* TLS-wrapped, even though the
KVM video port is (`kvmsecure=1`). (`vmedia.Options.TLS` honours `vmsecure` for
boards that set it.) Ports come from the jnlp: CD `cdport` **5120**, floppy
`fdport` **5122**, HD `hdport` **5123**; `singleportenabled=0` here (dedicated
ports, not tunnelled through the KVM port). `kvmtokentype`/token type 0 = web.

**32-byte IUSB header** (`IUSBHeader`), all multi-byte fields **little-endian**:

| off | size | field            | value / meaning                                  |
|-----|------|------------------|--------------------------------------------------|
| 0   | 8    | signature        | `"IUSB    "` (IUSB + 4 spaces)                    |
| 8   | 1    | major            | 1                                                |
| 9   | 1    | minor            | 0                                                |
| 10  | 1    | packetHeaderLen  | 32                                               |
| 11  | 1    | headerChecksum   | `(-Σ header bytes) & 0xFF` → receiver's Σ over the 32 header bytes is 0 |
| 12  | 4    | **dataPacketLen**| payload length — **the framing length**: read 32, then this many more |
| 16  | 1    | serverCaps       | 0                                                |
| 17  | 1    | deviceType       | CD-ROM = **5** (FD/HD types TBD)                 |
| 18  | 1    | protocol         | 1                                                |
| 19  | 1    | direction        | 128 on packets we send                           |
| 20  | 1    | deviceNumber     | 0                                                |
| 21  | 1    | interfaceNumber  | 0                                                |
| 22  | 1    | clientData       | 0                                                |
| 23  | 1    | Instance         | device instance # (which CD/FD/HD slot)          |
| 24  | 4    | sequenceNumber   | 0 on auth; echoed on responses                   |
| 28  | 4    | reserved         | 0                                                |

JViewer computes the checksum over the **header only** (the payload is written
*after* the checksum in its buffer, so the auth token is not covered) — so the BMC
validates at most the 32-byte header sum, never the payload. We match that.

**Payload / SCSI command envelope** (confirmed from live captures — vmedia is
plaintext, so requests are readable directly). After the 32-byte header the
payload is a ~29-byte command envelope; the **SCSI CDB starts at payload offset
9** (`opcode = data[9]`). For a 6-byte CDB the eject byte `data[13]` is CDB[4]; for
READ(10) the LBA is at `data[11:15]` (big-endian) and the block count at
`data[16:18]` (big-endian — a standard READ(10) CDB sitting at offset 9):

| payload off | field                                                |
|-------------|------------------------------------------------------|
| 0           | transfer/data length, u32 LE (bytes the host wants)  |
| 4           | command counter (increments per request)             |
| 8           | command marker `0x01`                                 |
| 9           | **SCSI CDB** (opcode … LBA@11:15 BE … len@16:18 BE)   |
| 25          | (response) length the BMC forwards to the host, LE    |

**Response framing.** The reply payload **echoes the request envelope and appends
the SCSI data-in bytes**, and must set the length field **at offset 25** to the
appended byte count — that is the field the BMC forwards to the host. (Setting only
offset 0 yields a zero-length read → `DID_ERROR` on the host. We set both.)
Status-only commands (TEST UNIT READY, …) echo the envelope with no data. The BMC
firmware answers enumeration (INQUIRY → "AMI Virtual CDROM0") itself and forwards
TEST UNIT READY as a ~30 s media-present poll plus the real data commands
(READ(10)/READ(12), READ CAPACITY, …) to us.

**Handshake** (`CDROMRedir.startRedirection` / `SendAuth_SessionToken`):

1. Client → **auth**: a 32 + 128-byte packet. `deviceType=5`, `Instance=dev#`,
   `dataPacketLen=128`; `data[9]=0xF2` (auth opcode); `data[30]=0`; the web
   **STOKEN** string at `data[31..]`. (Token type 0 = web session; type 1 = SSI,
   `dataPacketLen=240`.)
2. Server → **ACK** `data[9]=0xF1` (`DEVICE_REDIRECTION_ACK`). `connectionStatus`
   at `data[30]`: **1 = OK**, 5/8 = device error, anything else = already in use by
   the IP string at `data[31..]` (`m_otherIP`).
3. Loop: server sends IUSB SCSI **requests**, client emulates the SCSI/MMC command
   and sends an IUSB **response** (its own header + data) back. Eject = opcode
   `0x1B` (START STOP UNIT) with `data[13]==2`; kill = opcode `0xF6`.

Opcodes: `0xF2` auth · `0xF1` ack · `0xF6` kill-redir · `0x1B` start-stop-unit.

### Optional localhost turbo path

When the binary and browser are on the same machine, a future mode could let the
Go backend open the ISO directly (`os.File` + `ReadAt`), eliminating the WS
round-trip entirely. This is server-side file selection, not "through the
browser", so it would be an opt-in alternative, not the default.

## File layout

```
internal/power/power.go            IPMI chassis power control (RMCP+ 623)
internal/webui/control.go          /control WebSocket: JSON dispatch (power; vmedia stub)
internal/webui/webui.go            /control + /rd450x/ asset routes + vnc.html injection
internal/webui/assets/inject.js    toolbar UI (power panel, vmedia panel, read responder)
internal/webui/assets/inject.css   styling deltas on top of noVNC classes
internal/kvm/command.go            wires power.Controller → webui.ControlHandler
internal/kvm/vmedia/iusb.go        IUSB header codec, framing, opcodes, envelope offsets
internal/kvm/vmedia/auth.go        web-token auth packet + ACK/connection-status parse
internal/kvm/vmedia/session.go     plaintext/TLS connect, handshake, request/response loop
internal/kvm/vmedia/scsi.go        read-only CD-ROM SCSI/MMC emulation + echo-envelope response
internal/kvm/vmedia/reader.go      Reader interface + FileReader (localhost turbo path)
scripts/vmedia_probe_go/           bring-up driver: login → redirect a local ISO → serve (works)
```

**Done:** CD-ROM, floppy and HD/USB read paths (handshake, TUR, READ(10/12), READ
CAPACITY, MODE SENSE; CD adds the MMC probes) verified live with a backing
FileReader. **Next:** wire into the `kvm` command + browser read bridge (control
plane), the windowed LRU cache, and write support (WRITE(10)) for floppy/HD/USB.

## References

- AMI IUSB RE: <https://github.com/samozy/iusb> (framing, SCSI, floppy reference,
  Wireshark dissector) and <https://github.com/ya-mouse/redirector>
  (`com/ami/iusb/CDROMRedir.java`, `FloppyRedir.java` — CD-ROM + the 128 KiB / block-size constants).
- Browser on-demand model: OpenBMC virtual-media design
  <https://github.com/openbmc/docs/blob/master/designs/virtual-media.md> and
  `jsnbd` <https://github.com/openbmc/jsnbd> (`File.slice` + read-on-demand over WSS).
- W3C File API (slice laziness, snapshot-state errors):
  <https://w3c.github.io/FileAPI/>.
```
