# TODO

Consolidated backlog of planned work, gathered from `docs/`, in-code `TODO(kvm)`
markers, and bring-up notes. Completed work lives in git history; this file
tracks only what's still pending.

## KVM — feature parity with JViewer

- [x] **Power control buttons in noVNC** (the JViewer "Power" menu): Power On,
      ACPI Graceful Shutdown, Power Off (hard), Reset, Power Cycle. Done via an
      injected noVNC toolbar panel → `/control` WebSocket → `internal/power`
      (IPMI chassis control over RMCP+ UDP 623, `github.com/bougou/go-ipmi`).
      JViewer's six entries collapse onto five distinct IPMI commands ("Power Off"
      and "Immediate Shutdown" are the same hard power-down). _See `docs/kvm-vmedia.md`._
- [x] **Evaluate an alternative/extended frontend.** Decided: **extend stock
      noVNC**, don't replace it. No open KVM frontend speaks AMI's
      client-streams-sectors virtual-media model, so that data plane is ours
      regardless; adopting another frontend would also lose the working RFB video
      path. UI is added by injecting a `<script>` into `vnc.html` at serve time
      (submodule stays pristine), reusing noVNC's own CSS classes.
      _See `docs/kvm-vmedia.md`._
- [~] **Virtual media** (CD/FD/HD redirection, ports 5120/5122/5123) — mount
      local ISO/images to the host.
      - [x] **CD / Floppy / HD-USB data plane works end-to-end** (`internal/kvm/vmedia`):
        plaintext IUSB handshake + web-token auth; SCSI emulation — CD-ROM (MMC, 2048 B,
        `NewCDROM`, port 5120) and Direct-Access floppy/HD (SBC, 512 B, `NewDisk`, ports
        5122/5123); echo-envelope responses. Verified live (read-only): host mounts and
        reads `bin/test.iso` (iso9660), `bin/test-fd.img` (FAT12) and `bin/test-hd.img`
        (FAT16). Drive with `scripts/vmedia_probe_go -type cd|fd|hd`. Test media built by
        `scripts/mkiso_go` + `scripts/mkimg_go`. Protocol in `docs/kvm-vmedia.md`.
      - [x] Frontend + browser flow (toolbar panel, `File.slice` read responder).
      - [x] **Write support** (WRITE(10/12) + `NewDiskRW`, write-protect cleared) for
        floppy/HD/USB. Verified live: host mounts RW and files persist.
      - [x] **Physical-device passthrough** (Windows, raw access, needs elevation):
        `-dev Y:` for a single volume (lock+dismount) and `-disk Y:|N` for the WHOLE
        physical disk (`\\.\PhysicalDriveN`, taken offline; host sees the full GPT —
        groundwork for a WebUSB byte-source). Verified live read+write on a USB stick.
      - [x] **Wire the data plane into the `kvm` command + browser read bridge**
        (control plane): map `vmedia.attach/detach` → a `vmedia.Session`, back the
        `Reader` with the browser `File.slice` protocol; re-enable the toolbar entry.
      - [x] **Windowed LRU cache over the read protocol** (collapse round-trips):
        `vmedia.Cache` (`internal/kvm/vmedia/cache.go`) fronts the browser backing in
        `buildEmulator` — a miss fetches a 512 KiB aligned window (one `/control`
        round-trip) and serves subsequent reads from memory, bounded to 32 MiB via a
        64-window LRU; writes go through and invalidate touched windows. Hit rate is
        logged on detach.
      _Source: `CLAUDE.md` target-system notes; RE from decompiled JViewer + live BMC._

## KVM — video fidelity

- [ ] **Delta-frame / RC4 fidelity.** Live video "basically works" but pixel
      correctness isn't fully validated. Verify pass-2 delta tiles and the RC4
      `(byte)`-mask interpretation against captured live frames.
      _Source: memory `kvm-go-bridge-status`; `internal/kvm/codec/rc4.go` notes._

## KVM — input

- [ ] **Mouse-mode negotiation** (absolute vs relative). Currently fixed to
      absolute; negotiate with the BMC like JViewer does. _Source: memory._
- [ ] **KM (keyboard/mouse) encryption** — implement KMCrypt RC4 wrapping when
      the BMC enables KM encryption (HID report sizes grow to 49).
      _Source: `internal/kvm/hid.go:44`._
- [x] **International keyboard layouts (client-side).** Implemented as physical-key
      (scancode) pass-through: the frontend (`internal/webui/assets/js/keyboard.js`)
      maps each `KeyboardEvent.code` (layout-independent physical position) to its USB
      HID usage and ships it as a private "pass-through" keysym
      (`scancodePassthroughBase`, `internal/kvm/usbhid.go`); the sink decodes it
      straight to a usage so the **guest's** own keymap turns the key into a
      character — every layout works as long as the guest is set to it (e.g. the
      RD450X host's `us,ru` + Alt+Shift). Modifiers and unmapped keys fall through to
      noVNC's normal keysym path, so shortcuts/AltGr/paste are unaffected. Toggled
      (default on) from a Keyboard toolbar panel; patches noVNC's `_handleKeyEvent`
      locally with no extra server traffic. _Source: `internal/kvm/usbhid.go`,
      `internal/kvm/hid.go`, `internal/webui/assets/js/keyboard.js`._
- [ ] **Dispatch inbound control messages** (power status, keyboard-LED state,
      encryption negotiation) in the read loop instead of discarding them.
      _Source: `internal/kvm/client.go:207`._
