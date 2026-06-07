// Power panel: full chassis control (On / ACPI shutdown / Off / Reset / Cycle)
// over the /control WebSocket → IPMI. A coloured state dot polls the host while the
// panel is open and recovers on its own after a power command.

import { mkButton, mkPanel, mkHeading, mkActionButton } from "./dom.js";
import { register } from "./panel.js";
import { send, onMessage, onOpen } from "./control-socket.js";

const POWER_SVG = "app/images/power.svg";

// JViewer's six entries map onto five distinct IPMI chassis commands ("Power Off"
// and "Immediate Shutdown" are the same hard power-down).
// prettier-ignore
const POWER_ACTIONS = [
  { act: "on",    label: "Power On",    confirm: false },
  { act: "acpi",  label: "Shutdown",    confirm: true  },
  { act: "off",   label: "Power Off",   confirm: true  },
  { act: "reset", label: "Reset",       confirm: true  },
  { act: "cycle", label: "Power Cycle", confirm: true  },
];

const POWER_POLL_MS = 5000;

let lastPowerState = null; // "on" | "off" — last confirmed host power state
let pollTimer = null;

// build constructs the power button + panel and inserts them before `before` in
// `container`. Wires JSON handlers and the open/close-driven status polling.
export function build(container, before) {
  const btn = mkButton("rd450x_power_button", "Power control", POWER_SVG);
  const p = mkPanel("rd450x_power");

  p.panel.appendChild(mkHeading(POWER_SVG, "Power"));

  for (const a of POWER_ACTIONS) {
    p.panel.appendChild(
      mkActionButton(a.label, () => {
        if (a.confirm && !window.confirm(`Send power "${a.label}" to the host?`))
          return;
        setStatus("Sending…", "pending");
        send({ type: "power", action: a.act });
      }),
    );
  }

  const st = document.createElement("div");
  st.className = "rd450x_status";
  st.id = "rd450x_power_status";
  st.setAttribute("data-state", "unknown");
  renderStatus(st, "…");
  p.panel.appendChild(st);

  container.insertBefore(btn, before);
  container.insertBefore(p.wrap, before);

  register({
    panel: p.panel,
    btn,
    ids: ["rd450x_power", "rd450x_power_button"],
    onOpen: startPolling,
    onClose: stopPolling,
  });

  // JSON replies from the bridge.
  onMessage("power.status", (m) => {
    if (m.ok) {
      lastPowerState = m.on ? "on" : "off";
      setStatus(m.on ? "On" : "Off", lastPowerState);
    } else if (m.error !== "unavailable" && lastPowerState) {
      // Transient query failure (BMC briefly busy after a power command) — keep the
      // last known state visible; the next poll will refresh it.
      setStatus(lastPowerState === "on" ? "On" : "Off", lastPowerState, m.error);
    } else {
      setStatus("Unavailable", "unknown", m.error);
    }
  });
  onMessage("power.result", (m) => {
    if (m.ok) {
      setStatus(`Sent: ${m.action}`, "pending");
      setTimeout(refresh, 3000); // let the chassis settle
    } else {
      setStatus("Failed", "unknown", m.error);
    }
  });

  // Refresh once on (re)connect so the dot is correct even when the panel is shut.
  onOpen(refresh);
}

function refresh() {
  send({ type: "power.status" });
}

// Live status: poll while the panel is open so the dot tracks the host and recovers
// on its own after a power command. Polling stops when the panel closes — no need to
// probe the fragile BMC when no one is looking.
function startPolling() {
  refresh();
  if (pollTimer) clearInterval(pollTimer);
  pollTimer = setInterval(refresh, POWER_POLL_MS);
}

function stopPolling() {
  if (!pollTimer) return;
  clearInterval(pollTimer);
  pollTimer = null;
}

// setStatus updates the dot+label. state ∈ on|off|pending|unknown. Optional detail
// (e.g. an error) goes into the tooltip only — never innerHTML — to avoid XSS if the
// server ever echoes user-influenced text.
function setStatus(label, state, detail) {
  const e = document.getElementById("rd450x_power_status");
  if (!e) return;
  e.setAttribute("data-state", state ?? "unknown");
  e.title = detail ?? "";
  renderStatus(e, label);
}

// renderStatus paints a static dot + a text-node label into a status element.
function renderStatus(el, label) {
  el.textContent = "";
  const dot = document.createElement("span");
  dot.className = "rd450x_dot";
  el.appendChild(dot);
  el.appendChild(document.createTextNode(" " + label));
}
