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
- [ ] **Virtual media** (CD/FD/HD redirection, ports 5120/5122/5123) — mount
      local ISO/images to the host. **Frontend + browser flow done** (toolbar
      panel, `File.slice` on-demand read responder, control protocol); **the AMI
      IUSB data plane (`internal/kvm/vmedia`) is still to be written** — answer the
      BMC's SCSI reads and fetch bytes via the browser read protocol. Sizing and
      RE references in `docs/kvm-vmedia.md` (128 KiB max transfer; CD 2048/FD-HD
      512 B blocks; random access; port AMI IUSB from `samozy/iusb` +
      `ya-mouse/redirector`). _Source: `CLAUDE.md` target-system notes._

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
- [ ] **International keyboard layouts (client-side).** The HID keysym→USB map is
      US-layout only; non-ASCII (e.g. Cyrillic) is dropped on paste. Cyrillic
      *input* now works on the RD450X host via its console keymap (Alt+Shift
      toggle, `us,ru`), but the bridge itself still only emits US scancodes.
      _Source: `internal/kvm/usbhid.go`; 2026-06-06 session._
- [ ] **Dispatch inbound control messages** (power status, keyboard-LED state,
      encryption negotiation) in the read loop instead of discarding them.
      _Source: `internal/kvm/client.go:207`._
