// Virtual Media panel: pick a local image file and mount it on the host, read-only.
// The browser serves disk sectors on demand via File.slice — the whole image is
// never uploaded, only the bytes the host reads cross the wire. cd/fd/hd can be
// mounted in PARALLEL: each runs its own backing keyed by its device byte
// (control-socket.js routes binary sector requests by that byte).

import { mkButton, mkPanel, mkHeading, mkActionButton, fmtSize } from "./dom.js";
import { register } from "./panel.js";
import {
  send,
  onMessage,
  onIOError,
  setBacking,
  removeBacking,
  closeAllBackings,
  DEV,
} from "./control-socket.js";
import { fileBackingRO } from "./backings/file.js";

const DISC_SVG =
  "data:image/svg+xml," +
  encodeURIComponent(
    "<svg xmlns='http://www.w3.org/2000/svg' width='25' height='25' viewBox='0 0 24 24' fill='white'>" +
      "<path d='M12 2a10 10 0 100 20 10 10 0 000-20zm0 13a3 3 0 110-6 3 3 0 010 6z'/></svg>",
  );

// The three device kinds, in display order. `label` is the full name (select +
// messages); `tag` is the short code shown in the compact per-device status rows.
const KINDS = [
  { kind: "cd", label: "CD / DVD", tag: "CD" },
  { kind: "fd", label: "Floppy", tag: "FD" },
  { kind: "hd", label: "HDD / USB", tag: "HD" },
];

// Per-kind mount state, so we can show "already mounted" only for that one kind.
const state = { cd: {}, fd: {}, hd: {} };

// An image chosen but not yet mounted (one staging slot; mounting promotes it).
let pending = null;

// DOM refs filled by build().
let kindSel, fileInput;

export function build(container, before) {
  const btn = mkButton("rd450x_vmedia_button", "Virtual media", DISC_SVG);
  const p = mkPanel("rd450x_vmedia");

  p.panel.appendChild(mkHeading(DISC_SVG, "Virtual Media"));

  // Controls are stacked one per line, the same look as the Power panel.
  // Device kind.
  kindSel = document.createElement("select");
  kindSel.id = "rd450x_vmedia_kind";
  for (const k of KINDS) {
    const o = document.createElement("option");
    o.value = k.kind;
    o.textContent = k.label;
    kindSel.appendChild(o);
  }
  p.panel.appendChild(kindSel);

  // Source: a local image, read-only. A hidden <input type=file> does the picking;
  // File.slice streams sectors on demand, so multi-GB images are never uploaded.
  fileInput = document.createElement("input");
  fileInput.type = "file";
  fileInput.id = "rd450x_vmedia_file";
  fileInput.accept = ".iso,.img,.bin";
  fileInput.style.display = "none";
  fileInput.addEventListener("change", () => {
    const f = fileInput.files?.[0];
    if (f) setPending(fileBackingRO(f));
  });
  p.panel.appendChild(fileInput);

  // Choose / Mount / Unmount, stacked like the power actions.
  p.panel.appendChild(mkActionButton("Choose image…", () => fileInput.click()));
  p.panel.appendChild(mkActionButton("Mount", mountPending));
  p.panel.appendChild(mkActionButton("Unmount", unmountCurrent));

  // Transient one-line message (pick / I/O errors).
  const msg = document.createElement("div");
  msg.className = "rd450x_msg";
  msg.id = "rd450x_vmedia_msg";
  p.panel.appendChild(msg);

  // Per-device status list (cd / fd / hd).
  const list = document.createElement("div");
  list.className = "rd450x_devlist";
  for (const k of KINDS) {
    const row = document.createElement("div");
    row.className = "rd450x_status rd450x_devstatus";
    row.id = "rd450x_dev_" + k.kind;
    list.appendChild(row);
  }
  p.panel.appendChild(list);

  container.insertBefore(btn, before);
  container.insertBefore(p.wrap, before);

  // Paint the initial "not mounted" rows AFTER insertion — renderDevStatus resolves
  // rows via getElementById, which only sees nodes once they're in the document.
  for (const k of KINDS) renderDevStatus(k.kind, "idle", "not mounted");

  register({ panel: p.panel, btn, ids: ["rd450x_vmedia", "rd450x_vmedia_button"] });

  // JSON status replies, scoped to one kind. state ∈ mounted | unmounted | error.
  onMessage("vmedia.status", (m) => {
    const k = m.kind;
    if (!state[k]) return;
    if (m.state === "mounted") {
      state[k].mounted = true;
      renderDevStatus(k, "mounted", null, state[k].name, state[k].size);
    } else {
      // unmounted | error → close just this device's backing, leave others alone.
      removeBacking(DEV[k]);
      state[k].mounted = false;
      if (m.state === "error") {
        renderDevStatus(k, "error", "error");
        setMsg(`error (${labelOf(k)}): ${m.error || "unknown"}`);
      } else {
        renderDevStatus(k, "idle", "not mounted");
      }
    }
  });

  onIOError((e) => setMsg("I/O error: " + (e?.message || e)));

  // Drop any open backings if the page is closed mid-mount.
  window.addEventListener("pagehide", closeAllBackings);
}

// --- helpers ----------------------------------------------------------------

function labelOf(kind) {
  return KINDS.find((k) => k.kind === kind)?.label ?? kind;
}

function setPending(backing) {
  pending = backing;
  setMsg(`ready: ${backing.name} (${fmtSize(backing.size)}) — click Mount`);
}

// --- mount / unmount --------------------------------------------------------

function mountPending() {
  if (!pending) {
    setMsg("choose an image first");
    return;
  }
  const kind = kindSel.value;
  if (state[kind].mounted) {
    setMsg(`${labelOf(kind)} already mounted — unmount it first`);
    return;
  }
  const backing = pending;
  pending = null;

  // Register the backing under this kind's device byte so on-demand sector requests
  // route to it; remember its metadata for the status row once mounted.
  setBacking(DEV[kind], backing);
  state[kind].name = backing.name;
  state[kind].size = backing.size;
  renderDevStatus(kind, "mounting", "mounting…");

  send({ type: "vmedia.attach", kind, name: backing.name, size: backing.size });
}

function unmountCurrent() {
  const kind = kindSel.value;
  if (!state[kind].mounted) {
    setMsg(`${labelOf(kind)} is not mounted`);
    return;
  }
  send({ type: "vmedia.detach", kind });
  // The backing is closed when the server confirms (vmedia.status unmounted).
}

// --- status rendering -------------------------------------------------------

// renderDevStatus paints one device's row: a coloured dot (data-state ∈
// idle|mounting|mounted|error → CSS colour), the kind tag, and either a label
// ("not mounted" / "mounting…" / "error") or the mounted image name + size.
function renderDevStatus(kind, dataState, label, name, size) {
  const el = document.getElementById("rd450x_dev_" + kind);
  if (!el) return;
  el.setAttribute("data-state", dataState ?? "idle");
  el.textContent = "";

  const dot = document.createElement("span");
  dot.className = "rd450x_dot";
  el.appendChild(dot);

  const tag = document.createElement("span");
  tag.className = "rd450x_devtag";
  tag.textContent = KINDS.find((k) => k.kind === kind)?.tag ?? kind;
  el.appendChild(tag);

  const text = name ? `${name} (${fmtSize(size)})` : label ?? "not mounted";
  el.appendChild(document.createTextNode(text));
}

function setMsg(s) {
  const e = document.getElementById("rd450x_vmedia_msg");
  if (e) e.textContent = s;
}
