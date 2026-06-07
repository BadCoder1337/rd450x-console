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
internal/kvm/vmedia/               AMI IUSB sector-serving data plane — TODO
```

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
