// Keyboard panel: client-side international keyboard support via physical-key
// (scancode) pass-through.
//
// A KVM target sees raw USB HID usage codes (physical key positions) and applies
// its OWN keymap; it never sees characters. noVNC, by default, sends X11 keysyms
// derived from the typed character, which our bridge can only map for the US
// layout — so non-US keys (e.g. Cyrillic, German ü/ö, the swapped y/z) are
// dropped or mis-sent.
//
// This module fixes that by sending the physical key position instead of the
// character: we intercept noVNC's own key handling and, for every key in the
// layout-independent CODE_TO_USAGE table below, replace the keysym with a private
// "pass-through" keysym carrying the raw USB usage (scancodePassthroughBase in
// internal/kvm/usbhid.go). The guest's configured keymap then turns the physical
// key into the right character — so any layout works as long as the guest is set
// to it (e.g. the RD450X host's `us,ru` + Alt+Shift toggle for Cyrillic).
//
// Modifiers and any key not in the table fall through to noVNC's normal keysym
// path unchanged, so shortcuts (Ctrl/Alt/AltGr combos), the on-screen keyboard,
// and clipboard paste keep working. The mode is on by default and toggled from a
// small toolbar panel (persisted in localStorage).

import RFB from "../../core/rfb.js";
import { mkButton, mkPanel, mkHeading } from "./dom.js";
import { register } from "./panel.js";

const KEYBOARD_SVG = "app/images/keyboard.svg";
const LS_KEY = "rd450x_kbd_passthrough";

// Private keysym base; must match scancodePassthroughBase in internal/kvm/usbhid.go.
const PASS_BASE = 0xf0000000;

// CODE_TO_USAGE maps a KeyboardEvent.code (the physical key position, independent
// of the active layout) to its USB HID keyboard usage code. Values match the USB
// HID Usage Tables (and internal/kvm/usbhid.go's usbUsage). Modifier keys are
// deliberately omitted — they keep noVNC's standard modifier keysyms so the
// bridge's existing modifier-bit handling (incl. Windows AltGr → ISO_Level3_Shift)
// stays intact.
// prettier-ignore
const CODE_TO_USAGE = {
  // Letters
  KeyA: 0x04, KeyB: 0x05, KeyC: 0x06, KeyD: 0x07, KeyE: 0x08, KeyF: 0x09,
  KeyG: 0x0a, KeyH: 0x0b, KeyI: 0x0c, KeyJ: 0x0d, KeyK: 0x0e, KeyL: 0x0f,
  KeyM: 0x10, KeyN: 0x11, KeyO: 0x12, KeyP: 0x13, KeyQ: 0x14, KeyR: 0x15,
  KeyS: 0x16, KeyT: 0x17, KeyU: 0x18, KeyV: 0x19, KeyW: 0x1a, KeyX: 0x1b,
  KeyY: 0x1c, KeyZ: 0x1d,
  // Number row
  Digit1: 0x1e, Digit2: 0x1f, Digit3: 0x20, Digit4: 0x21, Digit5: 0x22,
  Digit6: 0x23, Digit7: 0x24, Digit8: 0x25, Digit9: 0x26, Digit0: 0x27,
  // Editing / whitespace
  Enter: 0x28, Escape: 0x29, Backspace: 0x2a, Tab: 0x2b, Space: 0x2c,
  // Punctuation
  Minus: 0x2d, Equal: 0x2e, BracketLeft: 0x2f, BracketRight: 0x30,
  Backslash: 0x31, Semicolon: 0x33, Quote: 0x34, Backquote: 0x35,
  Comma: 0x36, Period: 0x37, Slash: 0x38,
  CapsLock: 0x39,
  // Function row
  F1: 0x3a, F2: 0x3b, F3: 0x3c, F4: 0x3d, F5: 0x3e, F6: 0x3f,
  F7: 0x40, F8: 0x41, F9: 0x42, F10: 0x43, F11: 0x44, F12: 0x45,
  // System / navigation
  PrintScreen: 0x46, ScrollLock: 0x47, Pause: 0x48,
  Insert: 0x49, Home: 0x4a, PageUp: 0x4b, Delete: 0x4c, End: 0x4d, PageDown: 0x4e,
  ArrowRight: 0x4f, ArrowLeft: 0x50, ArrowDown: 0x51, ArrowUp: 0x52,
  NumLock: 0x53,
  // Keypad
  NumpadDivide: 0x54, NumpadMultiply: 0x55, NumpadSubtract: 0x56,
  NumpadAdd: 0x57, NumpadEnter: 0x58,
  Numpad1: 0x59, Numpad2: 0x5a, Numpad3: 0x5b, Numpad4: 0x5c, Numpad5: 0x5d,
  Numpad6: 0x5e, Numpad7: 0x5f, Numpad8: 0x60, Numpad9: 0x61, Numpad0: 0x62,
  NumpadDecimal: 0x63,
  // The extra key on 102-key ISO keyboards (between LeftShift and Z) and the
  // application/menu key — both common on European layouts.
  IntlBackslash: 0x64, ContextMenu: 0x65, NumpadEqual: 0x67,
  // Japanese / Brazilian extras.
  IntlRo: 0x87, IntlYen: 0x89,
};

let enabled = loadEnabled();

function loadEnabled() {
  try {
    return localStorage.getItem(LS_KEY) !== "0"; // default on
  } catch {
    return true;
  }
}

function saveEnabled(v) {
  try {
    localStorage.setItem(LS_KEY, v ? "1" : "0");
  } catch {
    /* private mode / disabled storage — keep the in-memory value */
  }
}

// patchRFB wraps RFB.sendKey once on the shared prototype. We patch sendKey
// rather than _handleKeyEvent on purpose: noVNC binds _handleKeyEvent to the
// instance at construction time (`onkeyevent = this._handleKeyEvent.bind(this)`,
// rfb.js), so patching it would only take effect if we won a race against
// `autoconnect` building the RFB — fragile. sendKey, by contrast, is invoked as
// `this.sendKey(...)` (dynamic dispatch through the prototype) on every key, so a
// prototype patch always applies regardless of when the instance was created, and
// it's a stable public API. It still carries the resolved physical `code` (after
// noVNC's AltGr-merge / key-tracking), so we just swap the keysym by position and
// delegate the wire write to the original. Modifiers and codes absent from
// CODE_TO_USAGE pass through untouched.
let patched = false;
function patchRFB() {
  if (patched) return;
  const proto = RFB && RFB.prototype;
  if (!proto || typeof proto.sendKey !== "function") {
    console.warn(
      "rd450x: noVNC RFB.sendKey unavailable — scancode pass-through disabled",
    );
    return;
  }
  const orig = proto.sendKey;
  proto.sendKey = function (keysym, code, down) {
    if (enabled) {
      const usage = CODE_TO_USAGE[code];
      if (usage !== undefined) keysym = (PASS_BASE | usage) >>> 0;
    }
    return orig.call(this, keysym, code, down);
  };
  patched = true;
}

// build constructs the keyboard button + panel and inserts them before `before`
// in `container`, then installs the RFB patch.
export function build(container, before) {
  const btn = mkButton(
    "rd450x_keyboard_button",
    "Keyboard layout",
    KEYBOARD_SVG,
  );
  const p = mkPanel("rd450x_keyboard");

  p.panel.appendChild(mkHeading(KEYBOARD_SVG, "Keyboard"));

  const row = document.createElement("label");
  row.className = "rd450x_kbd_row";
  const cb = document.createElement("input");
  cb.type = "checkbox";
  cb.id = "rd450x_kbd_passthrough";
  cb.checked = enabled;
  cb.addEventListener("change", () => {
    enabled = cb.checked;
    saveEnabled(enabled);
  });
  row.appendChild(cb);
  row.appendChild(
    document.createTextNode(" Pass physical keys to host (international layouts)"),
  );
  p.panel.appendChild(row);

  const hint = document.createElement("div");
  hint.className = "rd450x_kbd_hint";
  hint.textContent =
    "Sends each key's physical position; the host's own keyboard layout decides the character. Turn off to use the client layout (US scancodes).";
  p.panel.appendChild(hint);

  container.insertBefore(btn, before);
  container.insertBefore(p.wrap, before);

  register({
    panel: p.panel,
    btn,
    ids: ["rd450x_keyboard", "rd450x_keyboard_button"],
  });

  patchRFB();
}
