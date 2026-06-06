# TODO

Consolidated backlog of planned work, gathered from `docs/`, in-code `TODO(kvm)`
markers, and bring-up notes. Completed work lives in git history; this file
tracks only what's still pending.

## KVM — feature parity with JViewer

- [ ] **Power control buttons in noVNC** (the JViewer "Power" menu): Power On,
      Power Off, Immediate (hard) Shutdown, ACPI Graceful Shutdown, Reset, Power
      Cycle. Needs IPMI chassis power control — either RMCP+ over UDP 623
      (`github.com/bougou/go-ipmi`, already vendored for SOL) or the BMC web API
      (`opPowerControl`/`opPowerStatus`, IVTP 34/35, are already defined). Wire
      as a toolbar/panel in the embedded noVNC.
      _Source: user request; `internal/kvm/client.go` "dispatch control messages (power...)"._
- [ ] **Evaluate an alternative/extended frontend** for fuller JViewer parity
      (power, virtual media, recording, on-screen keyboard) instead of bending
      stock noVNC, if the button set grows large. _Source: user note._
- [ ] **Virtual media** (CD/FD/HD redirection, ports 5120/5122/5123) — mount
      remote ISO/images to the host. Not implemented. _Source: `CLAUDE.md` target-system notes._

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
