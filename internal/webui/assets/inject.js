// rd450x-console toolbar extension for noVNC.
//
// Injected into an otherwise-pristine noVNC page via <script defer src="rd450x/inject.js">.
// Adds two control-bar entries (Power, Virtual Media) that follow noVNC's own UI
// conventions, driven over the out-of-band /control WebSocket → IPMI.
//
// Power:         full chassis control (On / ACPI / Off / Reset / Cycle).
// Virtual Media: pick a local ISO/IMG and mount it on the host. The browser serves
//                disk sectors on demand via File.slice (lazy random access — the whole
//                image is never uploaded). The Go-side AMI IUSB data plane is still
//                pending (see docs/kvm-vmedia.md); the read responder is wired and inert.
//
// All control traffic goes to /control, never to the RFB video socket, so a power
// command can't stall the framebuffer.

(function () {
  "use strict";

  // ---- WebSocket to /control ------------------------------------------------

  const wsURL = `${location.protocol === "https:" ? "wss:" : "ws:"}//${location.host}/control`;
  let ws = null;
  let wsReady = false;
  const outbox = []; // messages queued until socket is open
  let lastPowerState = null; // "on" | "off" — last confirmed host power state

  function connect() {
    ws = new WebSocket(wsURL);
    ws.binaryType = "arraybuffer";
    ws.onopen = () => {
      wsReady = true;
      while (outbox.length) ws.send(outbox.shift());
      refreshPower();
    };
    ws.onclose = () => {
      wsReady = false;
      setTimeout(connect, 2000); // survive bridge restarts / page idle
    };
    ws.onerror = () => ws?.close();
    ws.onmessage = onMessage;
  }

  function send(obj) {
    const s = JSON.stringify(obj);
    if (wsReady) ws.send(s);
    else outbox.push(s);
  }

  function onMessage(ev) {
    if (typeof ev.data !== "string") {
      onBinaryRead(ev.data);
      return;
    }
    let m;
    try {
      m = JSON.parse(ev.data);
    } catch {
      return;
    }

    switch (m.type) {
      case "power.status":
        if (m.ok) {
          lastPowerState = m.on ? "on" : "off";
          setPowerStatus(m.on ? "On" : "Off", lastPowerState);
        } else if (m.error !== "unavailable" && lastPowerState) {
          // Transient query failure (BMC briefly busy after a power command) —
          // keep the last known state visible; the next poll will refresh it.
          setPowerStatus(
            lastPowerState === "on" ? "On" : "Off",
            lastPowerState,
            m.error,
          );
        } else {
          setPowerStatus("Unavailable", "unknown", m.error);
        }
        break;
      case "power.result":
        if (m.ok) {
          setPowerStatus(`Sent: ${m.action}`, "pending");
          setTimeout(refreshPower, 3000); // let the chassis settle
        } else {
          setPowerStatus("Failed", "unknown", m.error);
        }
        break;
      case "vmedia.status":
        setVmediaStatus(m.state + (m.error ? `: ${m.error}` : ""));
        break;
    }
  }

  // ---- Virtual-media read responder (on-demand sector serving) ---------------
  //
  // Wire protocol (binary, big-endian), used once the Go IUSB data plane lands:
  //   request  (server→browser): [u32 reqId][u64 offset][u32 len]   (16 bytes)
  //   response (browser→server): [u32 reqId][u8 status][bytes…]     (status 0=ok, 1=error)
  //
  // File.slice(offset, offset+len) reads ONLY that range lazily from disk, so a
  // multi-GB image is never loaded or uploaded in full.

  let vmediaFile = null;

  async function onBinaryRead(buf) {
    if (!vmediaFile) return;
    const dv = new DataView(buf);
    const reqId = dv.getUint32(0, false);
    // Reconstruct 64-bit offset from two 32-bit halves (JS lacks native u64).
    const offset = dv.getUint32(4, false) * 2 ** 32 + dv.getUint32(8, false);
    const len = dv.getUint32(12, false);
    try {
      const data = await vmediaFile.slice(offset, offset + len).arrayBuffer();
      const out = new Uint8Array(5 + data.byteLength);
      const odv = new DataView(out.buffer);
      odv.setUint32(0, reqId, false);
      odv.setUint8(4, 0); // status: ok
      out.set(new Uint8Array(data), 5);
      ws.send(out.buffer);
    } catch {
      // File changed or removed mid-mount — signal a read error to the server.
      const err = new Uint8Array(5);
      new DataView(err.buffer).setUint32(0, reqId, false);
      err[4] = 1; // status: error
      ws.send(err.buffer);
      setVmediaStatus("read error — image changed or removed");
    }
  }

  // ---- Panel helpers (noVNC conventions) ------------------------------------

  function closeAllPanels() {
    stopPowerPolling();
    const bar = document.getElementById("noVNC_control_bar");
    if (!bar) return;
    bar
      .querySelectorAll(".noVNC_panel.noVNC_open")
      .forEach((p) => p.classList.remove("noVNC_open"));
    bar
      .querySelectorAll(".noVNC_button.noVNC_selected")
      .forEach((b) => b.classList.remove("noVNC_selected"));
  }

  function closeOurPanels() {
    stopPowerPolling();
    for (const id of ["rd450x_power", "rd450x_vmedia"])
      document.getElementById(id)?.classList.remove("noVNC_open");
    for (const id of ["rd450x_power_button", "rd450x_vmedia_button"])
      document.getElementById(id)?.classList.remove("noVNC_selected");
  }

  function togglePanel(panel, btn, onOpen) {
    const open = panel.classList.contains("noVNC_open");
    closeAllPanels();
    if (!open) {
      panel.classList.add("noVNC_open");
      btn.classList.add("noVNC_selected");
      onOpen?.();
    }
  }

  // Clicking a noVNC button, the canvas, or anywhere outside our panels closes
  // them — mirrors noVNC's own one-panel-at-a-time behaviour.
  function onDocClick(e) {
    if (
      e.target.closest?.(
        "#rd450x_power,#rd450x_vmedia,#rd450x_power_button,#rd450x_vmedia_button",
      )
    )
      return;
    closeOurPanels();
  }

  function mkButton(id, title, src) {
    const b = document.createElement("input");
    b.type = "image";
    b.id = id;
    b.className = "noVNC_button";
    b.src = src;
    b.alt = title;
    b.title = title;
    return b;
  }

  function mkPanel(id) {
    const wrap = document.createElement("div");
    wrap.className = "noVNC_vcenter";
    const panel = document.createElement("div");
    panel.id = id;
    panel.className = "noVNC_panel";
    wrap.appendChild(panel);
    return { wrap, panel };
  }

  // ---- Power ----------------------------------------------------------------

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

  function buildPower(container, before) {
    const btn = mkButton("rd450x_power_button", "Power control", POWER_SVG);
    const p = mkPanel("rd450x_power");

    const head = document.createElement("div");
    head.className = "noVNC_heading";
    head.innerHTML = `<img alt="" src="${POWER_SVG}"> Power`;
    p.panel.appendChild(head);

    for (const a of POWER_ACTIONS) {
      const ab = document.createElement("input");
      ab.type = "button";
      ab.value = a.label;
      ab.addEventListener("click", () => {
        if (
          a.confirm &&
          !window.confirm(`Send power "${a.label}" to the host?`)
        )
          return;
        setPowerStatus("Sending…", "pending");
        send({ type: "power", action: a.act });
      });
      p.panel.appendChild(ab);
    }

    const st = document.createElement("div");
    st.className = "rd450x_status";
    st.id = "rd450x_power_status";
    st.setAttribute("data-state", "unknown");
    st.innerHTML = '<span class="rd450x_dot"></span>'; // static markup only — label added as text below
    st.appendChild(document.createTextNode(" …"));
    p.panel.appendChild(st);

    btn.addEventListener("click", () => {
      togglePanel(p.panel, btn, null);
      if (p.panel.classList.contains("noVNC_open")) startPowerPolling();
      else stopPowerPolling();
    });

    container.insertBefore(btn, before);
    container.insertBefore(p.wrap, before);
  }

  // Renders a coloured dot + label. state ∈ on|off|pending|unknown.
  // Optional detail (e.g. an error message) goes into the tooltip only — never
  // into innerHTML — to prevent XSS if the server ever echoes user-influenced text.
  function setPowerStatus(label, state, detail) {
    const e = document.getElementById("rd450x_power_status");
    if (!e) return;
    e.setAttribute("data-state", state ?? "unknown");
    e.title = detail ?? "";
    e.innerHTML = '<span class="rd450x_dot"></span>'; // static dot; label is text node
    e.appendChild(document.createTextNode(` ${label}`));
  }

  function refreshPower() {
    send({ type: "power.status" });
  }

  // Live status: poll while the power panel is open so the dot tracks the host
  // and recovers on its own after a power command. Polling stops when the panel
  // closes — no need to probe the BMC when no one is looking.
  const POWER_POLL_MS = 5000;
  let powerPollTimer = null;

  function startPowerPolling() {
    refreshPower();
    if (powerPollTimer) clearInterval(powerPollTimer);
    powerPollTimer = setInterval(refreshPower, POWER_POLL_MS);
  }

  function stopPowerPolling() {
    if (!powerPollTimer) return;
    clearInterval(powerPollTimer);
    powerPollTimer = null;
  }

  // ---- Virtual Media --------------------------------------------------------

  const DISC_SVG =
    "data:image/svg+xml," +
    encodeURIComponent(
      "<svg xmlns='http://www.w3.org/2000/svg' width='25' height='25' viewBox='0 0 24 24' fill='white'>" +
        "<path d='M12 2a10 10 0 100 20 10 10 0 000-20zm0 13a3 3 0 110-6 3 3 0 010 6z'/></svg>",
    );

  function buildVmedia(container, before) {
    const btn = mkButton("rd450x_vmedia_button", "Virtual media", DISC_SVG);
    const p = mkPanel("rd450x_vmedia");

    // Static panel markup — no user data interpolated here (file name/error are
    // set later via textContent), so innerHTML is safe for the structure.
    p.panel.innerHTML =
      `<div class="noVNC_heading"><img alt="" src="${DISC_SVG}"> Virtual Media</div>` +
      '<label for="rd450x_vmedia_file">Image (.iso / .img)</label>' +
      '<input type="file" id="rd450x_vmedia_file" accept=".iso,.img,.bin">' +
      '<select id="rd450x_vmedia_kind">' +
      '<option value="cd">CD / DVD</option>' +
      '<option value="fd">Floppy</option>' +
      '<option value="hd">HDD / USB</option>' +
      "</select>" +
      '<input type="button" id="rd450x_vmedia_mount" value="Mount">' +
      '<input type="button" id="rd450x_vmedia_unmount" value="Unmount">' +
      '<div class="rd450x_status" id="rd450x_vmedia_status">no media</div>';

    btn.addEventListener("click", () => togglePanel(p.panel, btn, null));

    container.insertBefore(btn, before);
    container.insertBefore(p.wrap, before);

    document
      .getElementById("rd450x_vmedia_mount")
      .addEventListener("click", () => {
        const f = document.getElementById("rd450x_vmedia_file").files?.[0];
        if (!f) {
          setVmediaStatus("choose a file first");
          return;
        }
        vmediaFile = f; // keep snapshot alive for on-demand sector reads
        const kind = document.getElementById("rd450x_vmedia_kind").value;
        setVmediaStatus(
          `mounting ${f.name} (${Math.round(f.size / 1048576)} MiB)…`,
        );
        send({ type: "vmedia.attach", kind, name: f.name, size: f.size });
      });

    document
      .getElementById("rd450x_vmedia_unmount")
      .addEventListener("click", () => {
        const kind = document.getElementById("rd450x_vmedia_kind").value;
        send({ type: "vmedia.detach", kind });
        vmediaFile = null;
        setVmediaStatus("unmounted");
      });
  }

  function setVmediaStatus(s) {
    const e = document.getElementById("rd450x_vmedia_status");
    if (e) e.textContent = s;
  }

  // ---- Init -----------------------------------------------------------------

  function init() {
    const bar = document.getElementById("noVNC_control_bar");
    if (!bar) return; // not the console page
    // The control-bar buttons live inside an inner scroll container, not the bar
    // itself. Insert ours next to the settings button, in that same parent.
    const before = document.getElementById("noVNC_settings_button");
    const container =
      before?.parentNode ?? bar.querySelector(".noVNC_scroll") ?? bar;
    buildPower(container, before);
    // Virtual Media is hidden until the AMI IUSB data plane lands (see
    // docs/kvm-vmedia.md); the panel/responder code above stays ready.
    // buildVmedia(container, before);
    document.addEventListener("click", onDocClick, false);
    connect();
  }

  if (document.readyState === "loading")
    document.addEventListener("DOMContentLoaded", init);
  else init();
})();
