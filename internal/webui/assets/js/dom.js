// Small DOM helpers shared across the toolbar modules. Kept dependency-free so
// every module can import them without pulling in panel/socket state.

// mkButton builds a noVNC control-bar icon button (the same <input type=image>
// noVNC uses for its own toolbar entries, so ours render identically).
export function mkButton(id, title, src) {
  const b = document.createElement("input");
  b.type = "image";
  b.id = id;
  b.className = "noVNC_button";
  b.src = src;
  b.alt = title;
  b.title = title;
  return b;
}

// mkPanel builds the noVNC slide-out panel wrapper (.noVNC_vcenter > .noVNC_panel)
// keyed by id. The returned { wrap, panel }: insert `wrap` into the control bar,
// fill `panel` with controls.
export function mkPanel(id) {
  const wrap = document.createElement("div");
  wrap.className = "noVNC_vcenter";
  const panel = document.createElement("div");
  panel.id = id;
  panel.className = "noVNC_panel";
  wrap.appendChild(panel);
  return { wrap, panel };
}

// mkHeading builds a noVNC panel heading (icon + label) matching the native panels.
export function mkHeading(svg, label) {
  const head = document.createElement("div");
  head.className = "noVNC_heading";
  const img = document.createElement("img");
  img.alt = "";
  img.src = svg;
  head.appendChild(img);
  head.appendChild(document.createTextNode(" " + label));
  return head;
}

// mkActionButton builds a plain push button (<input type=button>) used inside our
// panels for actions (Mount, Power On, …).
export function mkActionButton(label, onClick) {
  const b = document.createElement("input");
  b.type = "button";
  b.value = label;
  if (onClick) b.addEventListener("click", onClick);
  return b;
}

// fmtSize renders a byte count as a human-readable KiB/MiB/GiB string.
export function fmtSize(n) {
  if (n >= 1 << 30) return (n / (1 << 30)).toFixed(1) + " GiB";
  if (n >= 1 << 20) return Math.round(n / (1 << 20)) + " MiB";
  return Math.round(n / 1024) + " KiB";
}
