// rd450x-console toolbar extension for noVNC — ESM entry point.
//
// Loaded into an otherwise-pristine noVNC page via
// <script type="module" src="rd450x/js/main.js"> (injected by webui.go before
// </body>). It builds two control-bar entries that follow noVNC's own UI
// conventions, driven over the out-of-band /control WebSocket → IPMI:
//
//   Power:         full chassis control (On / ACPI / Off / Reset / Cycle).
//   Virtual Media: pick local images / WebUSB devices and mount them on the host;
//                  cd/fd/hd can be mounted in parallel, each served on demand
//                  (File.slice / WebUSB — never uploaded in full).
//
// All control traffic goes to /control, never to the RFB video socket, so a control
// command can't stall the framebuffer.

import { installOutsideClose } from "./panel.js";
import { connect } from "./control-socket.js";
import * as power from "./power.js";
import * as vmedia from "./vmedia.js";

function init() {
  const bar = document.getElementById("noVNC_control_bar");
  if (!bar) return; // not the console page

  // The control-bar buttons live inside an inner scroll container, not the bar
  // itself. Insert ours next to the settings button, in that same parent.
  const before = document.getElementById("noVNC_settings_button");
  const container = before?.parentNode ?? bar.querySelector(".noVNC_scroll") ?? bar;

  power.build(container, before);
  vmedia.build(container, before);

  installOutsideClose();
  connect();
}

// Module scripts defer by default, but guard the readyState anyway in case the DOM
// isn't ready (e.g. if the script is ever loaded differently).
if (document.readyState === "loading")
  document.addEventListener("DOMContentLoaded", init);
else init();
