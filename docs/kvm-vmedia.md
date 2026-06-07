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
                  ▼             RMCP+ 623)            browser-backed sectors)
                 BMC ◀──────────────┴───────────────────────┘
```

The **control plane is deliberately separate from the RFB video socket** so a
power command or a virtual-media sector read can never stall framebuffer updates.

## Injection (keeps the submodule pristine)

`internal/webui/novnc` (the noVNC v1.5.0 submodule) is **never modified**. At
serve time `webui.go` rewrites only the response for `/vnc.html`, splicing a
stylesheet + script tag in just before `</body>`:

```html
<link rel="stylesheet" href="rd450x/inject.css"><script type="module" src="rd450x/js/main.js"></script>
```

`internal/webui/assets/inject.css` + the ES modules under `internal/webui/assets/js/`
(a separate `go:embed assets/inject.css assets/js`) build the toolbar
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

## Virtual media — wired end-to-end (browser source ↔ Go data plane)

The toolbar's Virtual Media panel picks a local image file and sends an attach
control message; the Go side opens the AMI IUSB redirection and answers the BMC's
SCSI reads by pulling the needed sectors back from the browser on demand. The panel
mirrors the Power panel's look (a heading, stacked uniform-width controls, and a
per-device status list) and mounts **read-only**: a `<input type=file>` File is served
via `File.slice(offset, offset+len)`, so a multi-GB image is **never uploaded in
full** — only the sectors the host reads cross the wire (the model OpenBMC's Redfish
"proxy mode" uses). If the picked file changes mid-mount, `slice()` throws and the
responder reports an I/O error.

**cd/fd/hd mount in parallel:** each kind runs its own backing, keyed by a one-byte
device tag the server stamps into every sector request (see the wire protocol below).

**Read-only by design; writes go server-side.** The browser path serves **reads
only** — no WebUSB and no browser-side writes. (An earlier iteration added a WebUSB
mass-storage source and a File System Access read-write image; both were removed —
WebUSB mass storage is unclaimable on a stock Windows host without a driver swap, and
an FSA writable only commits on `close()`, breaking read-after-write within a mount.)
**Host writes go through the server-side path** instead: `scripts/vmedia_probe_go
-iso <img> -w` (or `-dev`/`-disk` for physical passthrough), which honours SCSI
WRITE(10/12) directly against the backing. The Go data plane and the `/control` wire
protocol keep their write op for that path; the browser simply never issues it.

**Limitation — mass storage only.** The AMI IUSB protocol redirects **mass-storage
devices** (CD/FD/HD SCSI) only; generic USB devices such as **hardware tokens**
(CCID/HID) cannot be redirected through it — that would need a separate
generic-USB-over-IP capability the MegaRAC does not expose here.

Binary wire protocol over `/control` (big-endian), implemented in
`assets/js/control-socket.js` (browser) and `internal/webui/control.go` +
`browserbacking.go` (Go). A one-byte op tag distinguishes reads from writes (writes
carry the host's data in the tail); a one-byte `dev` tag selects the target backing
so **cd/fd/hd can be mounted in parallel** over one control socket. The response
needs no `dev` byte — `reqId` is globally unique on the server side and routes it
back to the waiting request:

```
request  (server → browser): [u8 op][u8 dev][u32 reqId][u64 offset][u32 len][data… if write]
                             op  0 = read, 1 = write
                             dev 0 = cd,   1 = fd, 2 = hd   (kindToDev in control.go)
response (browser → server): [u32 reqId][u8 status][bytes… if read]   (status 0=ok, 1=error)
```

The JSON control plane carries the attach/detach lifecycle:

```jsonc
// browser → server  (the browser mounts read-only, so it never sets writable)
{ "type": "vmedia.attach", "kind": "cd|fd|hd", "name": "...", "size": 12345 }
{ "type": "vmedia.detach", "kind": "cd|fd|hd" }
// server → browser
{ "type": "vmedia.status", "kind": "...", "state": "mounted|unmounted|error", "error": "..." }
```

The browser-backed medium is a `vmedia.ReadWriter` (`internal/webui/browserbacking.go`)
whose `ReadAt`/`WriteAt` are one `/control` round-trip each. `internal/kvm/vmediactl.go`
**reuses the live KVM client's web session** rather than opening a new web login per
attach: the token minted for the video session (and the per-device ports + `vmsecure`
from the same jnlp) also authenticate the IUSB redirection. The KVM client therefore
no longer releases its web session after the video handshake (`internal/kvm/client.go`)
— it keeps it for vmedia reuse and logs out only on `Client.Close()` (KVM teardown),
which avoids hitting the fragile MegaRAC web stack again. Attach opens `vmedia.Connect`
and runs `Session.Serve` against the browser backing in a goroutine until detach (which
just stops the loop; the shared session is untouched). Several mounts (e.g. a CD and an
HD) can be active at once over one control socket; closing the tab detaches them all,
and the web session is released when the KVM client shuts down.

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
`go run ./scripts/mkimg_go`.

**Write support (floppy/HD/USB).** `-w` makes the backing writable: the data plane
honours SCSI **WRITE(10)/WRITE(12)** (`NewDiskRW`), clears the MODE SENSE
write-protect bit, and the host's write data rides at the **tail of the request
payload** (after the command envelope). Verified live: the remote host mounted the
device read-write and created files that persisted in the backing. (CD-ROM stays
read-only.)

**Physical-device passthrough** (`internal/kvm/vmedia/volume_windows.go`, raw
`\\.\…` via x/sys/windows; needs the probe **elevated (Administrator)** — the same
reason JViewer demands admin). Two granularities:

- `-dev Y:` — a single **volume** (`\\.\Y:`). Writable ⇒ the volume is
  **locked + dismounted**; remounted on close.
- `-disk Y:` (or `-disk 4`) — the **whole physical disk** (`\\.\PhysicalDriveN`),
  so the host sees the entire device incl. its partition table — the granularity a
  future **WebUSB** byte-source would expose. A drive letter is resolved to its
  disk number (`IOCTL_STORAGE_GET_DEVICE_NUMBER`). Writable ⇒ the disk is taken
  **offline** (`IOCTL_DISK_SET_DISK_ATTRIBUTES`, which dismounts all its volumes);
  back online on close.

Both verified live on a Kingston USB stick: read (the whole-disk path reads the
full GPT and all partitions) and write (the host created files that persisted on
the physical drive after Windows brought it back). Launch elevated, e.g.:
`Start-Process -Verb RunAs cmd '/c cd /d <repo> && bin\vmedia_probe.exe -disk Y: -type hd -w -duration 120s'`.
(The elevated process starts in System32, so `cd /d <repo>` is needed for `.env`.)

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
The whole `0xF0`–`0xFF` range is AMI redirection-control (not SCSI) — e.g. `0xF3` is
a periodic poll the BMC emits and `0xF4` is `SET_HARDDISK_TYPE`. They sit in the SCSI
opcode slot (`payload[9]`) but only need an empty echo-envelope ack; JViewer routes
them through its native SCSI handler, which does the same (`scsi.go` acks `0xF0+`
without a warning rather than treating them as unhandled SCSI).

### Optional localhost turbo path

When the binary and browser are on the same machine, a future mode could let the
Go backend open the ISO directly (`os.File` + `ReadAt`), eliminating the WS
round-trip entirely. This is server-side file selection, not "through the
browser", so it would be an opt-in alternative, not the default.

## File layout

```
internal/power/power.go            IPMI chassis power control (RMCP+ 623)
internal/webui/control.go          /control WebSocket: per-conn JSON+binary demux, vmedia attach/detach, request correlation
internal/webui/browserbacking.go   vmedia.ReadWriter backed by browser File.slice over /control (reads; write op kept for the server path)
internal/webui/webui.go            /control + /rd450x/ asset routes + vnc.html injection (Serve takes ControlHandler + VMediaController)
internal/webui/assets/js/          toolbar UI as ES modules (entry main.js):
                                     control-socket.js (/control WS + binary read plane),
                                     power.js, vmedia.js (read-only image mount, parallel cd/fd/hd, Power-panel look),
                                     backings/file.js (read-only <input type=file> backing),
                                     panel.js / dom.js (shared noVNC-panel helpers)
internal/webui/assets/inject.css   styling deltas on top of noVNC classes
internal/kvm/command.go            wires power.Controller → ControlHandler, vmediaControl → VMediaController
internal/kvm/vmediactl.go          VMediaController: reuses the KVM client's web session (token+ports+vmsecure), vmedia.Connect, Serve loop, detach
internal/kvm/vmedia/iusb.go        IUSB header codec, framing, opcodes, envelope offsets
internal/kvm/vmedia/auth.go        web-token auth packet + ACK/connection-status parse
internal/kvm/vmedia/session.go     plaintext/TLS connect, handshake, request/response loop
internal/kvm/vmedia/scsi.go        CD-ROM (MMC) + Direct-Access (floppy/HD) SCSI emulation, read+write
internal/kvm/vmedia/reader.go      Reader/ReadWriter + FileReader (localhost turbo path, RO/RW)
internal/kvm/vmedia/volume_windows.go  raw passthrough: volume (\\.\Y:, lock+dismount) & whole disk (\\.\PhysicalDriveN, offline)
scripts/vmedia_probe_go/           bring-up driver: -type cd|fd|hd, -iso/-dev/-disk, -w, -duration (works)
```

**Done:** CD-ROM, floppy and HD/USB read+write paths (handshake, TUR, READ(10/12),
WRITE(10/12), READ CAPACITY, MODE SENSE; CD adds the MMC probes) verified live with
both a file backing and raw physical-volume passthrough (`-dev Y:`, elevated). The
**browser control plane is wired**: the `kvm` command serves the vmedia panel
(read-only local image files, cd/fd/hd in parallel) and bridges the BMC's SCSI reads
to the browser on demand over `/control`; host writes use the server-side path.
**Next:** a windowed LRU/read-ahead cache (bootloader/ISO9660 reads are largely
sequential; collapsing many 128 KiB round-trips into larger aligned fetches would
cut latency). Tracked in `TODO.md`.

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
