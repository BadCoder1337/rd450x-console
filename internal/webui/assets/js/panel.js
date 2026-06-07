// Panel open/close coordination, mirroring noVNC's own one-panel-at-a-time UX:
// opening one of our panels (or a native noVNC button, or clicking the canvas)
// closes any other open panel.
//
// Panels register an optional onOpen/onClose pair so a panel can start/stop work
// (e.g. the power panel polls only while open).

// Registered panels: { panel, btn, ids, onOpen, onClose }.
const panels = [];

// register wires a panel into the coordinated open/close machinery and toggles it
// when its button is clicked. ids are the element ids (panel + button) used to tell
// "clicked inside our panel" from "clicked outside" in the document handler.
export function register({ panel, btn, ids, onOpen, onClose }) {
  panels.push({ panel, btn, ids, onOpen, onClose });
  btn.addEventListener("click", () => toggle(panel));
}

// toggle opens the given panel (closing all others) or closes it if already open.
function toggle(target) {
  const wasOpen = target.classList.contains("noVNC_open");
  closeOurs();
  if (!wasOpen) {
    for (const p of panels) {
      if (p.panel !== target) continue;
      p.panel.classList.add("noVNC_open");
      p.btn.classList.add("noVNC_selected");
      p.onOpen?.();
    }
  }
}

// closeOurs closes every panel we own, firing each panel's onClose.
export function closeOurs() {
  for (const p of panels) {
    if (p.panel.classList.contains("noVNC_open")) p.onClose?.();
    p.panel.classList.remove("noVNC_open");
    p.btn.classList.remove("noVNC_selected");
  }
}

// installOutsideClose closes our panels on any click that isn't inside one of them
// or on one of their buttons — matching noVNC's click-outside-to-close behaviour.
export function installOutsideClose() {
  document.addEventListener(
    "click",
    (e) => {
      const sel = panels
        .flatMap((p) => p.ids.map((id) => "#" + id))
        .join(",");
      if (sel && e.target.closest?.(sel)) return;
      closeOurs();
    },
    false,
  );
}
