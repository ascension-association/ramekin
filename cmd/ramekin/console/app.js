// Ramekin fleet dashboard. Device data comes from devices, so it is only ever
// inserted as text, never as HTML.
"use strict";

const key = document.querySelector('meta[name="ramekin-console-key"]').content;
const $ = (id) => document.getElementById(id);

const HEALTH = {
  healthy: "Healthy",
  warning: "Warning",
  updating: "Updating",
  unhealthy: "Unhealthy",
  idle: "No release",
  offline: "Offline",
};
const ORDER = ["unhealthy", "offline", "warning", "updating", "idle", "healthy"];

// Per-device UI state that must survive the 5-second refresh.
const confirming = new Map(); // name -> sha of the release to switch to
const pending = new Map(); // name -> version being switched to
const messages = new Map(); // name -> {ok, text}
const labels = new Map(); // name -> data: URL of the device's label SVG, while shown

let pollMs = 5000;

// Theme: dark unless the viewer picked light; remembered in this browser.
function applyTheme(theme) {
  if (theme === "light") document.documentElement.dataset.theme = "light";
  else delete document.documentElement.dataset.theme;
  const b = $("theme");
  b.setAttribute("aria-pressed", String(theme === "light"));
  b.textContent = theme === "light" ? "Dark theme" : "Light theme";
}
function savedTheme() {
  try { return localStorage.getItem("ramekin-console-theme"); } catch { return null; }
}
applyTheme(savedTheme());
$("theme").addEventListener("click", () => {
  const next = document.documentElement.dataset.theme === "light" ? "dark" : "light";
  try { localStorage.setItem("ramekin-console-theme", next); } catch {}
  applyTheme(next);
});

function el(tag, attrs = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") e.className = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (v !== undefined && v !== null && v !== false) e.setAttribute(k, v === true ? "" : v);
  }
  // Flatten fully: callers pass lists of lists, such as Details' [dt, dd] pairs.
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    e.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return e;
}

// Status icons: shape differs per state, so color never carries it alone.
const SVG = "http://www.w3.org/2000/svg";
function icon(health) {
  const s = document.createElementNS(SVG, "svg");
  s.setAttribute("viewBox", "0 0 16 16");
  s.setAttribute("class", "dot h-" + health);
  s.setAttribute("aria-hidden", "true");
  const shapes = {
    healthy: '<circle cx="8" cy="8" r="7" fill="currentColor"/><path d="M4.6 8.2l2.3 2.3 4.5-4.6" stroke="#fff" stroke-width="1.8" fill="none" stroke-linecap="round" stroke-linejoin="round"/>',
    warning: '<path d="M8 1.3l7 12.6H1z" fill="currentColor"/><path d="M8 6v3.6M8 11.6v.2" stroke="#0b0b0b" stroke-width="1.7" stroke-linecap="round"/>',
    updating: '<circle cx="8" cy="8" r="7" fill="currentColor"/><path d="M8 4.2v4l2.6 1.6" stroke="#0b0b0b" stroke-width="1.7" fill="none" stroke-linecap="round"/>',
    unhealthy: '<circle cx="8" cy="8" r="7" fill="currentColor"/><path d="M5.5 5.5l5 5m0-5l-5 5" stroke="#fff" stroke-width="1.8" stroke-linecap="round"/>',
    offline: '<circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" stroke-width="2"/><path d="M4 12L12 4" stroke="currentColor" stroke-width="2"/>',
    idle: '<circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" stroke-width="2" stroke-dasharray="3 2.4"/>',
  };
  s.innerHTML = shapes[health] || shapes.idle; // static markup only
  return s;
}

function badge(health) {
  return el("span", { class: "badge h-" + health }, icon(health), HEALTH[health] || health);
}

const GiB = 1024 ** 3;
function bytes(n) {
  if (n >= GiB) return (n / GiB).toFixed(n >= 10 * GiB ? 0 : 1) + " GiB";
  return Math.round(n / 1024 ** 2) + " MiB";
}

function ago(iso) {
  if (!iso) return "never";
  const s = Math.max(0, (Date.now() - Date.parse(iso)) / 1000);
  if (s < 10) return "just now";
  if (s < 90) return Math.round(s) + " s ago";
  if (s < 5400) return Math.round(s / 60) + " min ago";
  if (s < 129600) return Math.round(s / 3600) + " h ago";
  return Math.round(s / 86400) + " days ago";
}

function duration(sec) {
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
  if (d) return `${d} d ${h} h`;
  if (h) return `${h} h ${m} min`;
  return `${m} min`;
}

function meter(label, pct, text) {
  const level = pct >= 95 ? "full" : pct >= 80 ? "high" : "";
  const shown = Math.max(0, Math.min(100, pct));
  const fill = el("div", { class: "fill" });
  fill.style.width = shown + "%"; // CSSOM, which the console's CSP allows (style attributes it doesn't)
  return el("div", { class: "meter " + level },
    el("div", { class: "row" },
      el("span", {}, label),
      el("span", {}, text, level && el("span", { class: "flag" }, level === "full" ? "critical" : "high"))),
    el("div", {
      class: "track", role: "meter", "aria-label": label,
      "aria-valuemin": "0", "aria-valuemax": "100", "aria-valuenow": shown.toFixed(0),
    }, fill));
}

function origin(r) {
  if (!r.source) return "";
  return r.source === "push" ? "push" : "channel " + r.source.replace("channel:", "");
}

// The device's releases: the running one, then the earlier ones it kept
// (newest first), each of which it can switch to.
function versions(d, st, offline, busy) {
  const kept = st.history || (st.previous ? [st.previous] : []);
  const rows = [];
  const row = (r, state, action) =>
    el("li", { class: "version " + state },
      el("div", { class: "v" },
        el("span", { class: "vname" }, r.version),
        el("span", { class: "vmeta" }, [origin(r), r.installed_at && "installed " + ago(r.installed_at)].filter(Boolean).join(" · "))),
      action);
  if (st.trial) rows.push(row(st.trial, "trial", el("span", { class: "tag" }, "health-checking")));
  if (st.current) rows.push(row(st.current, "current", el("span", { class: "tag" }, "running")));
  const sha = confirming.get(d.name);
  for (const r of kept) {
    const can = !offline && !busy && !st.trial;
    rows.push(row(r, "kept", el("button", {
      disabled: !can,
      "aria-label": `Switch ${d.name} to ${r.version}`,
      title: offline ? "The device is offline" : busy || st.trial ? "The device is busy" : undefined,
      onclick: () => { confirming.set(d.name, r.sha256); messages.delete(d.name); render(); },
    }, sha === r.sha256 ? "Selected" : "Switch")));
  }
  const list = el("ol", { class: "versions", "aria-label": "Releases" }, rows);
  if (!kept.length && st.current) list.append(el("li", { class: "none" }, "No earlier releases kept yet."));
  return list;
}

function card(d) {
  const st = d.status;
  const m = st && st.metrics;
  const offline = d.health === "offline";
  const busy = d.busy || pending.has(d.name);

  const meters = m
    ? el("div", { class: "meters" },
        meter("CPU", m.cpu_percent, m.cpu_percent.toFixed(0) + "%"),
        meter("Memory", m.mem_total_bytes ? (100 * m.mem_used_bytes) / m.mem_total_bytes : 0,
          `${bytes(m.mem_used_bytes)} of ${bytes(m.mem_total_bytes)}`),
        meter("Disk (/perm)", m.disk_total_bytes ? (100 * m.disk_used_bytes) / m.disk_total_bytes : 0,
          `${bytes(m.disk_used_bytes)} of ${bytes(m.disk_total_bytes)}`))
    : el("p", { class: "nodata" }, offline ? "No recent resource data." : "This device doesn't report resource use yet.");

  const facts = [];
  if (st) {
    facts.push(["Arch", st.arch]);
    if (st.channel) facts.push(["Last update check", ago(st.last_check)]);
    if (m) facts.push(["Uptime", duration(m.uptime_seconds)]);
    if (st.app && st.app.since) facts.push([st.app.running ? "App up since" : "App down since", ago(st.app.since)]);
    if (st.app && st.app.restarts) facts.push(["App restarts", String(st.app.restarts)]);
    if (st.app && st.app.last_exit) facts.push(["Last exit", st.app.last_exit]);
    if (st.bad_count) facts.push(["Rejected releases", String(st.bad_count)]);
    if (st.firmware) {
      let fw = st.firmware.version || "unversioned";
      if (st.firmware.pending) fw += ` (installing ${st.firmware.pending})`;
      facts.push(["Firmware", fw]);
      const failed = Object.keys(st.firmware.failed || {});
      if (failed.length) facts.push(["Firmware that failed", failed.join(", ")]);
    }
  }
  if (d.gokrazy_note) facts.push(["gokrazy dashboard", d.gokrazy_note]);
  if (d.short_id) facts.push(["ID (on its label)", d.short_id]);
  if (d.uuid) facts.push(["UUID", d.uuid]);
  facts.push(["Identity", d.verified ? "verified (signed by its claimed key)" : "not claimed"]);
  facts.push(["API", d.url || "—"]);
  facts.push([offline ? "Last answered" : "Polled", offline ? ago(d.seen) : ago(d.polled)]);

  const kept = (st && (st.history || (st.previous ? [st.previous] : []))) || [];
  const target = kept.find((r) => r.sha256 === confirming.get(d.name));
  if (!target) confirming.delete(d.name);
  const actions = el("div", { class: "actions" });
  if (busy) {
    const to = pending.get(d.name) || (d.busy || "").replace("switching to ", "");
    actions.append(el("p", { class: "msg" }, `Switching to ${to}… the device is health-checking it.`));
  } else if (target && !offline) {
    actions.append(
      el("p", { class: "confirm" }, `Switch ${d.name} from ${st.current.version} to ${target.version}? The device health-checks ${target.version} first and keeps ${st.current.version} if it fails.`),
      el("button", { class: "danger", onclick: () => switchTo(d.name, target) }, `Switch to ${target.version}`),
      el("button", { onclick: () => { confirming.delete(d.name); render(); } }, "Cancel"));
  }
  const msg = messages.get(d.name);
  if (msg && !busy) actions.append(el("p", { class: "msg " + (msg.ok ? "ok" : "err"), role: "status" }, msg.text));
  const qr = labels.get(d.name);
  if (qr) {
    actions.append(el("figure", { class: "label" },
      el("img", { src: qr, alt: `QR code of ${d.name}'s ID, ${d.short_id}`, width: "270" }),
      el("figcaption", {},
        el("a", { href: qr, download: `${d.name}-label.svg` }, "Download SVG"),
        el("button", { type: "button", onclick: () => { labels.delete(d.name); render(); } }, "Hide"))));
  }

  const showing = logs && logs.name === d.name;
  return el("article", { class: "card h-" + d.health + (offline ? " offline" : ""), "aria-label": d.name },
    el("div", { class: "head" },
      el("div", {}, el("h2", { class: "name" }, d.name), el("div", { class: "where" }, st && st.channel ? "channel " + st.channel : "development device")),
      el("div", { class: "head-right" },
        badge(d.health),
        el("div", { class: "card-buttons" }, // Logs, then GK right beside it, even when the header wraps
          el("button", {
            class: "logs-btn", type: "button", "aria-pressed": String(!!showing),
            "aria-label": `Show ${d.name}'s logs`,
            onclick: () => (showing ? closeLogs() : openLogs(d.name)),
          }, "Logs"),
          d.gokrazy && el("button", {
            class: "logs-btn", type: "button",
            title: `gokrazy dashboard: ${d.gokrazy_note}`,
            "aria-label": `Open ${d.name}'s gokrazy dashboard (new tab)`,
            onclick: () => openGokrazy(d.name),
          }, "GK")))),
    d.detail && el("p", { class: "detail" }, d.detail),
    st ? versions(d, st, offline, busy) : el("p", { class: "nodata" }, "No release information yet."),
    meters,
    el("details", {}, el("summary", {}, "Details"),
      el("dl", {}, facts.map(([k, v]) => [el("dt", {}, k), el("dd", {}, v)])),
      d.uuid && !qr && el("button", { type: "button", class: "qr-btn", onclick: () => showLabel(d.name) }, "Show QR code")),
    actions.childElementCount ? actions : null);
}

let fleet = null;
const openDetails = new Set();

function render() {
  if (!fleet) return;
  // Keep expanded "Details" open across refreshes.
  document.querySelectorAll("article.card").forEach((c) => {
    const n = c.getAttribute("aria-label");
    if (c.querySelector("details")?.open) openDetails.add(n); else openDetails.delete(n);
  });
  const devices = [...fleet.devices].sort((a, b) =>
    ORDER.indexOf(a.health) - ORDER.indexOf(b.health) || a.name.localeCompare(b.name));
  $("devices").replaceChildren(...devices.map(card));
  document.querySelectorAll("article.card").forEach((c) => {
    if (openDetails.has(c.getAttribute("aria-label"))) c.querySelector("details").open = true;
  });
  $("empty").hidden = devices.length > 0;

  const counts = {};
  for (const d of devices) counts[d.health] = (counts[d.health] || 0) + 1;
  const tiles = [el("div", { class: "tile" }, el("div", { class: "n" }, devices.length), el("div", { class: "l" }, "Devices"))];
  for (const h of ["healthy", "warning", "updating", "unhealthy", "offline", "idle"]) {
    if (!counts[h] && h !== "healthy") continue;
    tiles.push(el("div", { class: "tile h-" + h }, el("div", { class: "n" }, counts[h] || 0), el("div", { class: "l" }, icon(h), HEALTH[h])));
  }
  $("summary").replaceChildren(...tiles);
}

async function api(path, opts = {}) {
  const r = await fetch(path, { ...opts, headers: { "X-Ramekin-Console": key } });
  if (!r.ok) throw new Error((await r.text()).trim() || r.statusText);
  return r.json();
}

async function refresh() {
  try {
    fleet = await api("/api/fleet");
    pollMs = (fleet.interval_seconds || 5) * 1000;
    $("banner").hidden = true;
    $("updated").textContent = "updated " + new Date().toLocaleTimeString();
    render();
  } catch (e) {
    $("banner").textContent = "Lost contact with the dashboard (" + e.message + "). Is `ramekin --dashboard` still running?";
    $("banner").hidden = false;
  }
}

async function switchTo(name, r) {
  confirming.delete(name);
  pending.set(name, r.version);
  render();
  try {
    const q = new URLSearchParams({ sha: r.sha256, version: r.version });
    const res = await api(`/api/devices/${encodeURIComponent(name)}/switch?${q}`, { method: "POST" });
    messages.set(name, res.ok ? { ok: true, text: `Done: ${name} is running ${res.version}.` } : { ok: false, text: res.error });
  } catch (e) {
    messages.set(name, { ok: false, text: e.message });
  }
  pending.delete(name);
  await refresh();
}

// Live logs: one device at a time, in a panel outside the cards so the
// 5-second refresh doesn't reset it. Log lines come from the device's app,
// so they are only ever inserted as text.
const MAX_LOG_LINES = 2000;
const ANSI = /\x1b\[[0-9;?]*[ -\/]*[@-~]/g; // colors and cursor moves
let logs = null; // {name, ctrl}

function setLogState(text, err = false) {
  $("logs-state").textContent = text;
  $("logs-state").classList.toggle("err", err);
}

function appendLogLines(lines) {
  const body = $("logs-body");
  const atBottom = body.scrollHeight - body.scrollTop - body.clientHeight < 24;
  const frag = document.createDocumentFragment();
  for (const l of lines) frag.append(el("div", { class: "line" }, l.replace(ANSI, "").replace(/\r$/, "")));
  body.append(frag);
  while (body.childElementCount > MAX_LOG_LINES) body.firstChild.remove();
  if (atBottom) body.scrollTop = body.scrollHeight; // follow, unless the viewer scrolled up
}

async function streamLogs(name, ctrl) {
  setLogState("Connecting…");
  $("logs-reconnect").hidden = true;
  let partial = "";
  try {
    const r = await fetch(`/api/devices/${encodeURIComponent(name)}/logs`, { headers: { "X-Ramekin-Console": key }, signal: ctrl.signal });
    if (!r.ok) throw new Error((await r.text()).trim() || r.statusText);
    setLogState("Following");
    const reader = r.body.pipeThrough(new TextDecoderStream()).getReader();
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      const parts = (partial + value).split("\n");
      partial = parts.pop();
      if (parts.length) appendLogLines(parts);
    }
    if (partial) appendLogLines([partial]);
    setLogState("The device closed the log stream.", true);
  } catch (e) {
    if (ctrl.signal.aborted) return;
    setLogState("Disconnected: " + e.message, true);
  }
  if (!ctrl.signal.aborted) $("logs-reconnect").hidden = false;
}

// Opening (or reconnecting) starts fresh: the device replays its recent lines first.
function openLogs(name) {
  if (logs) logs.ctrl.abort();
  logs = { name, ctrl: new AbortController() };
  $("logs-title").textContent = "Logs · " + name;
  $("logs-body").replaceChildren();
  $("logs").hidden = false;
  document.body.classList.add("logs-open");
  render();
  streamLogs(name, logs.ctrl);
}

function closeLogs() {
  if (!logs) return;
  logs.ctrl.abort();
  logs = null;
  $("logs").hidden = true;
  document.body.classList.remove("logs-open");
  render();
}

$("logs-close").addEventListener("click", closeLogs);
$("logs-clear").addEventListener("click", () => $("logs-body").replaceChildren());
$("logs-reconnect").addEventListener("click", () => logs && openLogs(logs.name));
$("logs").addEventListener("keydown", (e) => { if (e.key === "Escape") closeLogs(); });

// GK: the console opens the device's gokrazy dashboard on a loopback port of
// its own and logs in there, so no password reaches this page. The tab is
// opened right away (browsers block pop-ups opened after a delay), then sent
// to the one-time login link once the console has it.
async function openGokrazy(name) {
  const tab = window.open("about:blank", "_blank");
  if (tab) tab.opener = null; // the gokrazy page can't reach back into this one
  try {
    const res = await api(`/api/devices/${encodeURIComponent(name)}/gokrazy`, { method: "POST" });
    if (!res.ok) throw new Error(res.error);
    if (!/^http:\/\//.test(res.url)) throw new Error("unexpected address " + res.url);
    if (tab) tab.location.replace(res.url);
    else window.open(res.url, "_blank", "noopener");
  } catch (e) {
    if (tab) tab.close();
    messages.set(name, { ok: false, text: `Couldn't open ${name}'s gokrazy dashboard: ${e.message}` });
    render();
  }
}

// The device's sticker, as made by `ramekin device label`. It's shown as an
// image (scripts in an SVG image never run) from a data: URL, which the
// console's CSP allows for images.
async function showLabel(name) {
  try {
    const r = await fetch(`/api/devices/${encodeURIComponent(name)}/label.svg`, { headers: { "X-Ramekin-Console": key } });
    if (!r.ok) throw new Error((await r.text()).trim() || r.statusText);
    const svg = await r.text();
    labels.set(name, "data:image/svg+xml;base64," + btoa(unescape(encodeURIComponent(svg))));
  } catch (e) {
    messages.set(name, { ok: false, text: `Couldn't show ${name}'s QR code: ${e.message}` });
  }
  render();
}

async function loop() {
  await refresh();
  setTimeout(loop, pollMs);
}
loop();
// Relative times ("12 s ago") tick between polls.
setInterval(render, 15000);
