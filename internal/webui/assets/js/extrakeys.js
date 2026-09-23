// Extra keys: arrow keys and F1..F12 in noVNC's own "Extra keys" drawer.
//
// BIOS setup and boot menus are driven by arrows and function keys, which a
// browser tab often swallows (F5 reloads, F11 goes full screen, F1 opens help).
// The drawer already has Ctrl/Alt/Windows toggles, Tab, Esc and Ctrl-Alt-Del;
// this adds, always visible below them, the arrows as a keyboard's inverted T and
// F1..F12 as a 3x4 grid. The held Ctrl/Alt toggles still apply, so e.g. Alt + F4
// works.
//
// Keys go through noVNC's UI.sendKey (a press and release) with the physical key
// code, so the scancode pass-through in keyboard.js handles them like typed keys.

import UI from "../../app/ui.js";
import KeyTable from "../../core/input/keysym.js";

// arrowSvg is a white arrow pointing up, rotated by deg. Static markup only.
function arrowSvg(deg) {
  return (
    "<svg width='14' height='14' viewBox='0 0 14 14' aria-hidden='true'>" +
    `<g transform='rotate(${deg} 7 7)' fill='none' stroke='white' stroke-width='2' ` +
    "stroke-linecap='round' stroke-linejoin='round'>" +
    "<path d='M7 12V2M2.5 6.5L7 2l4.5 4.5'/></g></svg>"
  );
}

// [name, grid area, rotation, keysym, code]
const ARROWS = [
  ["Up", "up", 0, KeyTable.XK_Up, "ArrowUp"],
  ["Left", "left", 270, KeyTable.XK_Left, "ArrowLeft"],
  ["Down", "down", 180, KeyTable.XK_Down, "ArrowDown"],
  ["Right", "right", 90, KeyTable.XK_Right, "ArrowRight"],
];

function mkKey(title, keysym, code) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = "rd450x_key";
  b.title = title;
  b.addEventListener("click", () => UI.sendKey(keysym, code));
  return b;
}

export function build() {
  const drawer = document.getElementById("noVNC_modifiers");
  if (!drawer) return;

  const box = document.createElement("div");
  box.id = "rd450x_keys";

  const arrows = document.createElement("div");
  arrows.className = "rd450x_arrows";
  for (const [name, area, deg, keysym, code] of ARROWS) {
    const b = mkKey("Send " + name, keysym, code);
    b.style.gridArea = area;
    b.innerHTML = arrowSvg(deg);
    arrows.appendChild(b);
  }

  const fkeys = document.createElement("div");
  fkeys.className = "rd450x_fkeys";
  for (let n = 1; n <= 12; n++) {
    const b = mkKey("Send F" + n, KeyTable["XK_F" + n], "F" + n);
    b.textContent = "F" + n;
    fkeys.appendChild(b);
  }

  box.append(arrows, fkeys);
  drawer.appendChild(box);
}
