package main

// dashboardHTML is served at the plugin resource path. It is deliberately one
// self-contained document: no external scripts, fonts, or stylesheets, because
// the resource route is unauthenticated and must not pull third-party code.
//
// The page renders its own JSON calls against /v0/management/speedometer/snapshot.
// It can reuse the management key the control panel already saved in this
// origin's localStorage, but only after the operator clicks the button that
// offers it: the resource route is unauthenticated, so the page never spends a
// stored credential without an explicit action. The key is held in memory and
// is not written back to storage. The host bans an IP for 30 minutes after five
// rejected keys, so the page never retries a key the host has rejected.
const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Token Speedometer</title>
<style>
  :root {
    color-scheme: light;
    --surface-1: #fcfcfb;
    --page: #f9f9f7;
    --text-primary: #0b0b0b;
    --text-secondary: #52514e;
    --text-muted: #898781;
    --grid: #e1e0d9;
    --axis: #c3c2b7;
    --border: rgba(11,11,11,0.10);
    --series-1: #2a78d6;
    --series-2: #eb6834;
    --critical: #d03b3b;
    --warning: #fab219;
    --good: #0ca30c;
  }
  @media (prefers-color-scheme: dark) {
    :root:not([data-theme="light"]) {
      color-scheme: dark;
      --surface-1: #1a1a19;
      --page: #0d0d0d;
      --text-primary: #f0efec;
      --text-secondary: #c3c2b7;
      --text-muted: #898781;
      --grid: #2c2c2a;
      --axis: #383835;
      --border: rgba(255,255,255,0.10);
      --series-1: #3987e5;
      --series-2: #d95926;
    }
  }
  :root[data-theme="dark"] {
    color-scheme: dark;
    --surface-1: #1a1a19;
    --page: #0d0d0d;
    --text-primary: #f0efec;
    --text-secondary: #c3c2b7;
    --text-muted: #898781;
    --grid: #2c2c2a;
    --axis: #383835;
    --border: rgba(255,255,255,0.10);
    --series-1: #3987e5;
    --series-2: #d95926;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    padding: 24px 16px 48px;
    background: var(--page);
    color: var(--text-primary);
    font: 14px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif;
  }
  .wrap { max-width: 1080px; margin: 0 auto; display: grid; gap: 16px; }
  header { display: flex; flex-wrap: wrap; gap: 12px; align-items: baseline; justify-content: space-between; }
  h1 { font-size: 15px; font-weight: 600; margin: 0; letter-spacing: 0.01em; }
  .meta { color: var(--text-muted); font-size: 12px; }
  .card {
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: 10px;
    padding: 16px;
  }
  .hero { display: flex; flex-wrap: wrap; gap: 24px; align-items: flex-end; }
  .hero-main { display: grid; gap: 2px; }
  .hero-value { font-size: 44px; line-height: 1.05; font-weight: 600; letter-spacing: -0.02em; }
  .hero-unit { color: var(--text-secondary); font-size: 13px; }
  .stats { display: flex; flex-wrap: wrap; gap: 24px; }
  .stat { display: grid; gap: 2px; min-width: 96px; }
  .stat-value { font-size: 18px; font-weight: 600; font-variant-numeric: tabular-nums; }
  .stat-label { color: var(--text-muted); font-size: 11px; text-transform: uppercase; letter-spacing: 0.04em; }
  .card-head { display: flex; flex-wrap: wrap; gap: 8px; align-items: baseline; justify-content: space-between; margin-bottom: 12px; }
  h2 { font-size: 12px; font-weight: 600; margin: 0; text-transform: uppercase; letter-spacing: 0.04em; color: var(--text-secondary); }
  .legend { display: flex; gap: 14px; flex-wrap: wrap; font-size: 12px; color: var(--text-secondary); }
  .legend span { display: inline-flex; gap: 6px; align-items: center; }
  .swatch { width: 10px; height: 10px; border-radius: 2px; display: inline-block; }
  .chart-host { position: relative; }
  svg { display: block; width: 100%; height: auto; overflow: visible; }
  .tip {
    position: absolute; pointer-events: none; opacity: 0; transition: opacity 80ms linear;
    background: var(--surface-1); border: 1px solid var(--border); border-radius: 8px;
    padding: 8px 10px; font-size: 12px; color: var(--text-primary);
    box-shadow: 0 4px 16px rgba(0,0,0,0.12); white-space: nowrap; z-index: 3;
  }
  .tip b { font-variant-numeric: tabular-nums; }
  .cols { display: grid; gap: 16px; grid-template-columns: repeat(auto-fit, minmax(300px, 1fr)); }
  /* Grid and flex both default to min-width:auto, which lets a long unbroken
     credential id widen its card past the column instead of ellipsising. */
  .cols > .card { min-width: 0; }
  .bars { display: grid; gap: 10px; min-width: 0; }
  .bar-row { display: grid; gap: 4px; min-width: 0; }
  .bar-top { display: flex; justify-content: space-between; gap: 12px; font-size: 12px; min-width: 0; }
  .bar-name { flex: 1 1 auto; min-width: 0; color: var(--text-primary); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .bar-val { flex: 0 0 auto; color: var(--text-secondary); font-variant-numeric: tabular-nums; white-space: nowrap; }
  .bar-track { height: 8px; background: var(--grid); border-radius: 4px; overflow: hidden; }
  .bar-fill { height: 100%; border-radius: 4px; background: var(--series-1); }
  .empty { color: var(--text-muted); font-size: 13px; padding: 6px 0; }
  table { width: 100%; border-collapse: collapse; font-size: 12px; }
  th, td { text-align: left; padding: 6px 8px; border-bottom: 1px solid var(--grid); }
  th { color: var(--text-muted); font-weight: 500; text-transform: uppercase; font-size: 10px; letter-spacing: 0.04em; }
  td.num, th.num { text-align: right; font-variant-numeric: tabular-nums; }
  tbody tr:last-child td { border-bottom: none; }
  .gate { display: grid; gap: 10px; max-width: 460px; }
  .gate p { margin: 0; color: var(--text-secondary); font-size: 13px; }
  .gate label { font-size: 12px; color: var(--text-secondary); }
  input[type="password"], input[type="text"] {
    width: 100%; padding: 8px 10px; font: inherit; color: var(--text-primary);
    background: var(--page); border: 1px solid var(--border); border-radius: 8px;
  }
  button {
    font: inherit; padding: 8px 14px; border-radius: 8px; cursor: pointer;
    background: var(--series-1); color: #fff; border: 1px solid transparent;
  }
  button.ghost { background: transparent; color: var(--text-secondary); border-color: var(--border); }
  .row { display: flex; gap: 8px; flex-wrap: wrap; align-items: center; }
  .status { font-size: 12px; color: var(--text-muted); }
  .status.err { color: var(--critical); }
  .hidden { display: none !important; }
  .toolbar { display: flex; gap: 8px; flex-wrap: wrap; align-items: center; }
</style>
</head>
<body>
<div class="wrap">

  <header>
    <h1>Token speedometer</h1>
    <div class="meta" id="meta">connecting</div>
  </header>

  <section class="card gate hidden" id="gate">
    <p id="gate-explain">The control panel did not leave a management key in this
       browser, so the speedometer needs one to read the management API. It is used only
       for the snapshot request to this server.</p>
    <div class="row hidden" id="inherit-row">
      <button id="inherit">Use the control panel's saved key</button>
    </div>
    <label for="key">Management key</label>
    <input id="key" type="password" autocomplete="current-password" placeholder="management key">
    <div class="row">
      <button id="connect">Connect</button>
      <button class="ghost" id="forget">Forget key</button>
      <span class="status" id="gate-status"></span>
    </div>
    <p class="status">Five wrong keys ban this IP for 30 minutes, so the page
       stops after any rejection instead of retrying.</p>
  </section>

  <section class="card hidden" id="live">
    <div class="hero">
      <div class="hero-main">
        <div class="hero-value" id="rate-total">0</div>
        <div class="hero-unit">tokens generated / second</div>
      </div>
      <div class="stats">
        <div class="stat">
          <div class="stat-value" id="rate-committed">0</div>
          <div class="stat-label">committed</div>
        </div>
        <div class="stat">
          <div class="stat-value" id="rate-live">0</div>
          <div class="stat-label">live estimate</div>
        </div>
        <div class="stat">
          <div class="stat-value" id="streams">0</div>
          <div class="stat-label">streams open</div>
        </div>
        <div class="stat">
          <div class="stat-value" id="window-tokens">0</div>
          <div class="stat-label" id="window-label">tokens in window</div>
        </div>
        <div class="stat">
          <div class="stat-value" id="window-reqs">0</div>
          <div class="stat-label" id="reqs-label">requests in window</div>
        </div>
      </div>
    </div>
  </section>

  <section class="card hidden" id="chart-card">
    <div class="card-head">
      <h2>Generated tokens per second over time</h2>
      <div class="legend">
        <span><i class="swatch" style="background:var(--series-1)"></i>committed</span>
        <span><i class="swatch" style="background:var(--series-2)"></i>live estimate</span>
      </div>
    </div>
    <div class="chart-host" id="chart-host">
      <svg id="chart" role="img" aria-label="Tokens per second over the recent window, split into committed and live estimated tokens"></svg>
      <div class="tip" id="tip"></div>
    </div>
  </section>

  <div class="cols hidden" id="dims">
    <section class="card" id="card-provider">
      <div class="card-head"><h2>By provider</h2></div>
      <div class="bars" id="bars-provider"></div>
    </section>
    <section class="card" id="card-model">
      <div class="card-head"><h2>By model</h2></div>
      <div class="bars" id="bars-model"></div>
    </section>
    <section class="card" id="card-client">
      <div class="card-head"><h2>By client</h2></div>
      <div class="bars" id="bars-client"></div>
    </section>
    <section class="card" id="card-auth">
      <div class="card-head"><h2>By credential</h2></div>
      <div class="bars" id="bars-auth"></div>
    </section>
  </div>

  <section class="card hidden" id="inflight-card">
    <div class="card-head">
      <h2>Streams in flight</h2>
      <span class="status" id="inflight-count"></span>
    </div>
    <table>
      <thead>
        <tr>
          <th>Provider</th><th>Model</th><th>Client</th>
          <th class="num">Elapsed</th><th class="num">Tokens</th><th class="num">Tok/s</th>
        </tr>
      </thead>
      <tbody id="inflight-body"></tbody>
    </table>
    <div class="empty hidden" id="inflight-empty">No streams open.</div>
  </section>

  <section class="card hidden" id="table-card">
    <div class="card-head">
      <h2>Totals</h2>
      <div class="toolbar">
        <span class="status" id="updated"></span>
        <button class="ghost" id="toggle-table">Table view</button>
      </div>
    </div>
    <div id="table-wrap" class="hidden">
      <table>
        <thead><tr><th>Scope</th><th class="num">Tokens</th><th class="num">Requests</th><th class="num">Input</th><th class="num">Output</th></tr></thead>
        <tbody id="table-body"></tbody>
      </table>
    </div>
  </section>

</div>
<script>
(function () {
  "use strict";

  var FORGET_STORE = "cpa-token-speedometer-forget";
  var POLL_MS = 2000;
  var state = { key: null, timer: null, stopped: false, series: [], dims: null };

  var el = function (id) { return document.getElementById(id); };
  var fmt = function (n) {
    if (n === null || n === undefined || isNaN(n)) return "0";
    var v = Math.abs(n);
    if (v >= 1e9) return (n / 1e9).toFixed(2) + "B";
    if (v >= 1e6) return (n / 1e6).toFixed(2) + "M";
    if (v >= 1e3) return (n / 1e3).toFixed(1) + "k";
    if (v >= 100) return String(Math.round(n));
    if (v >= 10) return n.toFixed(1);
    return n.toFixed(2).replace(/\.00$/, "");
  };
  var fmtDur = function (s) {
    if (s < 60) return s.toFixed(1) + "s";
    var m = Math.floor(s / 60);
    return m + "m " + Math.round(s % 60) + "s";
  };

  // The resource lives at /v0/resource/plugins/<id>/ while the JSON route is
  // under /v0/management/, so derive the sibling path instead of hardcoding it.
  function snapshotURL() {
    var path = location.pathname;
    var marker = "/resource/plugins/";
    var idx = path.indexOf(marker);
    if (idx < 0) return null;
    return path.slice(0, idx) + "/management/speedometer/snapshot";
  }

  // --- panel session -----------------------------------------------------
  // The control panel persists its login as JSON under "cli-proxy-auth",
  // obfuscated with a repeating XOR of a fixed salt plus this origin's host.
  // The plugin page is same-origin with the API whenever the panel is served by
  // the proxy itself, so the key the operator already entered is readable here.
  // This mirrors the panel's own storage format; it is obfuscation, not
  // encryption, and grants nothing the panel did not already expose to this
  // origin.

  var PANEL_STORE = "cli-proxy-auth";
  var PANEL_SALT = "cli-proxy-api-webui::secure-storage";

  function panelUnxor(bytes, salt) {
    var key = new TextEncoder().encode(salt);
    var out = new Uint8Array(bytes.length);
    for (var i = 0; i < bytes.length; i++) out[i] = bytes[i] ^ key[i % key.length];
    return out;
  }

  function panelDecode(raw) {
    var v2 = "enc::v2::", v1 = "enc::v1::";
    var legacy = raw.slice(0, v1.length) === v1;
    if (!legacy && raw.slice(0, v2.length) !== v2) return raw; // stored plaintext
    try {
      var text = atob(raw.slice(v1.length));
      var bytes = new Uint8Array(text.length);
      for (var i = 0; i < text.length; i++) bytes[i] = text.charCodeAt(i);
      var salt = PANEL_SALT + "|" + location.host + (legacy ? "|" + navigator.userAgent : "");
      return new TextDecoder().decode(panelUnxor(bytes, salt));
    } catch (e) {
      return null;
    }
  }

  function panelStoredKey() {
    var raw = null;
    try { raw = localStorage.getItem(PANEL_STORE); } catch (e) { return null; }
    if (!raw) return null;
    var text = panelDecode(raw);
    if (!text) return null;
    var parsed = null;
    try { parsed = JSON.parse(text); } catch (e) { return null; }
    var holders = [parsed, parsed && parsed.state];
    for (var i = 0; i < holders.length; i++) {
      var s = holders[i];
      if (!s || typeof s.managementKey !== "string" || !s.managementKey) continue;
      // The panel can be pointed at another server. Only reuse its key when the
      // session it belongs to is this very origin, so a leftover key for a
      // different proxy is never spent against this host's failure ban.
      if (typeof s.apiBase === "string" && s.apiBase) {
        var origin = null;
        try { origin = new URL(s.apiBase, location.href).origin; } catch (e) { origin = null; }
        if (origin && origin !== location.origin) return null;
      }
      return s.managementKey;
    }
    return null;
  }

  function show(id, on) { el(id).classList.toggle("hidden", !on); }

  function stopPolling(reason, isError) {
    state.stopped = true;
    if (state.timer) { clearTimeout(state.timer); state.timer = null; }
    var s = el("gate-status");
    s.textContent = reason;
    s.className = "status" + (isError ? " err" : "");
  }

  function connect(key) {
    state.key = key;
    state.stopped = false;
    el("gate-status").textContent = "";
    show("gate", false);
    show("live", true); show("chart-card", true);
    show("dims", true); show("inflight-card", true); show("table-card", true);
    poll();
  }
  function poll() {
    if (state.stopped) return;
    var url = snapshotURL();
    if (!url) { stopPolling("Cannot find the management route from this page URL.", true); return; }
    fetch(url, {
      method: "GET",
      headers: { "Authorization": "Bearer " + state.key, "Accept": "application/json" },
      credentials: "omit",
      cache: "no-store"
    }).then(function (resp) {
      if (resp.status === 401 || resp.status === 403 || resp.status === 429) {
        // Never retry a rejected key: the host bans the IP after five failures.
        return resp.text().then(function (body) {
          var msg = "Key rejected";
          try { var j = JSON.parse(body); if (j && j.error) msg = j.error; } catch (e) {}
          lockOut(msg);
        });
      }
      if (!resp.ok) throw new Error("HTTP " + resp.status);
      return resp.json().then(render).then(schedule);
    }).catch(function (err) {
      // A transport failure is transient; keep trying but say so.
      el("meta").textContent = "reconnecting: " + err.message;
      schedule();
    });
  }

  function lockOut(message) {
    stopPolling(message + ". Reload the page to try again.", true);
    show("gate", true);
    ["live", "chart-card", "dims", "inflight-card", "table-card"].forEach(function (id) { show(id, false); });
  }

  function schedule() {
    if (state.stopped) return;
    if (state.timer) clearTimeout(state.timer);
    state.timer = setTimeout(poll, POLL_MS);
  }

  function render(payload) {
    var d = payload.data || {};
    var p = payload.plugin || {};
    var rate = d.rate || { committed: 0, live: 0, total: 0 };

    el("rate-total").textContent = fmt(rate.total);
    el("rate-committed").textContent = fmt(rate.committed);
    el("rate-live").textContent = fmt(rate.live);
    el("streams").textContent = fmt(d.streams || 0);
    el("window-tokens").textContent = fmt((d.window || {}).tokens || 0);
    el("window-reqs").textContent = fmt((d.window || {}).requests || 0);
    el("window-label").textContent = "tokens in " + fmtDur(d.window_seconds || 0);
    el("reqs-label").textContent = "requests in " + fmtDur(d.window_seconds || 0);
    el("meta").textContent = (p.name || "speedometer") + " " + (p.version || "") +
      " · uptime " + fmtDur(d.uptime_seconds || 0) +
      " · window " + fmtDur(d.window_seconds || 0);
    el("updated").textContent = "updated " + new Date().toLocaleTimeString();

    state.series = d.series || [];
    state.dims = d.dimensions || {};
    drawChart(state.series);
    renderDims(state.dims);
    renderInflight(d.inflight || []);
    renderTable(d);
  }

  function renderDims(dims) {
    ["provider", "model", "client", "auth"].forEach(function (dim) {
      var host = el("bars-" + dim);
      var card = el("card-" + dim);
      var rows = dims[dim] || [];
      host.textContent = "";
      if (!rows.length) {
        card.classList.add("hidden");
        return;
      }
      card.classList.remove("hidden");
      var max = 0;
      rows.forEach(function (r) { max = Math.max(max, (r.counters || {}).generated || 0); });
      rows.slice(0, 8).forEach(function (r) {
        var tokens = (r.counters || {}).generated || 0;
        var label = r.label || r.name || "unknown";
        var row = document.createElement("div");
        row.className = "bar-row";

        var top = document.createElement("div");
        top.className = "bar-top";
        var name = document.createElement("span");
        name.className = "bar-name";
        name.textContent = label;
        name.title = label;
        var val = document.createElement("span");
        val.className = "bar-val";
        val.textContent = fmt(r.rate || 0) + " tok/s";
        top.appendChild(name); top.appendChild(val);

        var track = document.createElement("div");
        track.className = "bar-track";
        var fill = document.createElement("div");
        fill.className = "bar-fill";
        fill.style.width = (max > 0 ? Math.max(2, (tokens / max) * 100) : 0) + "%";
        track.appendChild(fill);

        row.appendChild(top); row.appendChild(track);
        host.appendChild(row);
      });
    });
  }

  function renderInflight(rows) {
    var body = el("inflight-body");
    body.textContent = "";
    el("inflight-count").textContent = rows.length ? rows.length + " open" : "";
    show("inflight-empty", rows.length === 0);
    rows.slice(0, 12).forEach(function (r) {
      var tr = document.createElement("tr");
      var cells = [
        r.provider || "—",
        r.model || "—",
        r.client_label || shortHash(r.client),
        fmtDur(r.elapsed_seconds || 0),
        fmt((r.counters || {}).tokens || 0),
        fmt(r.tokens_per_second || 0)
      ];
      cells.forEach(function (text, i) {
        var td = document.createElement("td");
        if (i >= 3) td.className = "num";
        td.textContent = text;
        tr.appendChild(td);
      });
      body.appendChild(tr);
    });
  }

  function shortHash(hash) {
    if (!hash) return "—";
    if (hash === "__unknown__") return "unknown";
    if (hash === "__other__") return "other";
    return hash.slice(0, 8);
  }

  function renderTable(d) {
    var body = el("table-body");
    body.textContent = "";
    var rows = [
      ["Window", d.window],
      ["Since restart", d.lifetime]
    ];
    var dims = d.dimensions || {};
    Object.keys(dims).forEach(function (dim) {
      // The bars rank by generated tokens, which is the rate they print. This
      // table prints billed tokens instead, so it sorts by that column rather
      // than inheriting an order that would read out of sequence here.
      var rows_ = (dims[dim] || []).slice().sort(function (a, b) {
        var at = (a.counters || {}).tokens || 0;
        var bt = (b.counters || {}).tokens || 0;
        if (at !== bt) return bt - at;
        return (a.label || a.name || "") < (b.label || b.name || "") ? -1 : 1;
      });
      rows_.forEach(function (r) {
        rows.push([dim + ": " + (r.label || r.name || "unknown"), r.counters]);
      });
    });
    rows.forEach(function (pair) {
      var c = pair[1] || {};
      var tr = document.createElement("tr");
      [pair[0], fmt(c.tokens || 0), fmt(c.requests || 0), fmt(c.input || 0), fmt(c.output || 0)]
        .forEach(function (text, i) {
          var td = document.createElement("td");
          if (i > 0) td.className = "num";
          td.textContent = text;
          tr.appendChild(td);
        });
      body.appendChild(tr);
    });
  }

  // --- chart -------------------------------------------------------------

  var PAD = { top: 10, right: 8, bottom: 22, left: 44 };
  var SVG_H = 200;

  function drawChart(series) {
    var svg = el("chart");
    svg.textContent = "";
    if (!series.length) return;

    var host = el("chart-host");
    var width = Math.max(320, host.clientWidth || 640);
    var innerW = width - PAD.left - PAD.right;
    var innerH = SVG_H - PAD.top - PAD.bottom;

    var maxY = 1;
    series.forEach(function (p) {
      maxY = Math.max(maxY, p.committed + p.live);
    });
    // Round the top up so the gridline labels stay readable.
    maxY = niceMax(maxY);

    svg.setAttribute("viewBox", "0 0 " + width + " " + SVG_H);
    svg.setAttribute("height", String(SVG_H));

    var ns = "http://www.w3.org/2000/svg";
    var x = function (i) {
      return PAD.left + (series.length === 1 ? innerW / 2 : (i / (series.length - 1)) * innerW);
    };
    var y = function (v) { return PAD.top + innerH - (v / maxY) * innerH; };

    // Gridlines and y labels.
    for (var g = 0; g <= 4; g++) {
      var v = (maxY / 4) * g;
      var gy = y(v);
      var line = document.createElementNS(ns, "line");
      line.setAttribute("x1", String(PAD.left));
      line.setAttribute("x2", String(PAD.left + innerW));
      line.setAttribute("y1", String(gy));
      line.setAttribute("y2", String(gy));
      line.setAttribute("stroke", "var(--grid)");
      line.setAttribute("stroke-width", "1");
      svg.appendChild(line);

      var label = document.createElementNS(ns, "text");
      label.setAttribute("x", String(PAD.left - 8));
      label.setAttribute("y", String(gy + 3));
      label.setAttribute("text-anchor", "end");
      label.setAttribute("fill", "var(--text-muted)");
      label.setAttribute("font-size", "10");
      label.textContent = fmt(v);
      svg.appendChild(label);
    }

    // X labels: first and last timestamps only, to avoid collisions.
    [[0, "start"], [series.length - 1, "end"]].forEach(function (pair) {
      var idx = pair[0];
      var p = series[idx];
      if (!p) return;
      var t = document.createElementNS(ns, "text");
      t.setAttribute("x", String(x(idx)));
      t.setAttribute("y", String(SVG_H - 6));
      t.setAttribute("text-anchor", idx === 0 ? "start" : "end");
      t.setAttribute("fill", "var(--text-muted)");
      t.setAttribute("font-size", "10");
      t.textContent = new Date(p.ts * 1000).toLocaleTimeString();
      svg.appendChild(t);
    });

    // Committed and live are drawn as two stacked bands: committed from the
    // baseline up to its line, live from the committed line up to the total. The
    // live band must close on the committed line, not on the baseline, or it
    // repaints the committed area and the two blend into a muddy middle.
    var liveArea = document.createElementNS(ns, "path");
    liveArea.setAttribute("d",
      linePath(series, function (p) { return p.committed + p.live; }, x, y) + " " +
      reversePath(series, function (p) { return p.committed; }, x, y) + "Z");
    liveArea.setAttribute("fill", "var(--series-2)");
    liveArea.setAttribute("fill-opacity", "0.55");
    svg.appendChild(liveArea);

    var committedArea = document.createElementNS(ns, "path");
    committedArea.setAttribute("d", closeToBaseline(series, function (p) { return p.committed; }, x, y));
    committedArea.setAttribute("fill", "var(--series-1)");
    committedArea.setAttribute("fill-opacity", "0.55");
    svg.appendChild(committedArea);

    var totalLine = document.createElementNS(ns, "path");
    totalLine.setAttribute("d", linePath(series, function (p) { return p.committed + p.live; }, x, y));
    totalLine.setAttribute("fill", "none");
    totalLine.setAttribute("stroke", "var(--series-2)");
    totalLine.setAttribute("stroke-width", "2");
    totalLine.setAttribute("stroke-linejoin", "round");
    svg.appendChild(totalLine);

    var committedLine = document.createElementNS(ns, "path");
    committedLine.setAttribute("d", linePath(series, function (p) { return p.committed; }, x, y));
    committedLine.setAttribute("fill", "none");
    committedLine.setAttribute("stroke", "var(--series-1)");
    committedLine.setAttribute("stroke-width", "2");
    committedLine.setAttribute("stroke-linejoin", "round");
    svg.appendChild(committedLine);

    // Hover layer: a crosshair plus a tooltip on the nearest second.
    var crosshair = document.createElementNS(ns, "line");
    crosshair.setAttribute("y1", String(PAD.top));
    crosshair.setAttribute("y2", String(PAD.top + innerH));
    crosshair.setAttribute("stroke", "var(--axis)");
    crosshair.setAttribute("stroke-width", "1");
    crosshair.setAttribute("opacity", "0");
    svg.appendChild(crosshair);

    var hit = document.createElementNS(ns, "rect");
    hit.setAttribute("x", String(PAD.left));
    hit.setAttribute("y", String(PAD.top));
    hit.setAttribute("width", String(innerW));
    hit.setAttribute("height", String(innerH));
    hit.setAttribute("fill", "transparent");
    svg.appendChild(hit);

    var tip = el("tip");

    hit.addEventListener("mousemove", function (ev) {
      var box = svg.getBoundingClientRect();
      var ratio = box.width ? width / box.width : 1;
      var px = (ev.clientX - box.left) * ratio;
      var frac = (px - PAD.left) / innerW;
      var idx = Math.round(frac * (series.length - 1));
      idx = Math.max(0, Math.min(series.length - 1, idx));
      var p = series[idx];
      if (!p) return;

      var cx = x(idx);
      crosshair.setAttribute("x1", String(cx));
      crosshair.setAttribute("x2", String(cx));
      crosshair.setAttribute("opacity", "1");

      var total = p.committed + p.live;
      tip.innerHTML = "";
      var when = document.createElement("div");
      when.textContent = new Date(p.ts * 1000).toLocaleTimeString();
      when.style.color = "var(--text-secondary)";
      when.style.marginBottom = "4px";
      tip.appendChild(when);
      [["Total", total], ["Committed", p.committed], ["Live", p.live]].forEach(function (pair) {
        var row = document.createElement("div");
        row.style.display = "flex";
        row.style.justifyContent = "space-between";
        row.style.gap = "14px";
        var k = document.createElement("span");
        k.textContent = pair[0];
        k.style.color = "var(--text-secondary)";
        var v = document.createElement("b");
        v.textContent = fmt(pair[1]) + " tok/s";
        row.appendChild(k); row.appendChild(v);
        tip.appendChild(row);
      });

      var left = cx / ratio;
      tip.style.opacity = "1";
      var tipW = tip.offsetWidth || 140;
      var hostW = host.clientWidth || width;
      tip.style.left = Math.max(0, Math.min(hostW - tipW, left + 12)) + "px";
      tip.style.top = "8px";
    });

    hit.addEventListener("mouseleave", function () {
      crosshair.setAttribute("opacity", "0");
      tip.style.opacity = "0";
    });
  }

  function niceMax(v) {
    var mag = Math.pow(10, Math.floor(Math.log10(v || 1)));
    var step = mag;
    var steps = [1, 2, 2.5, 5, 10];
    for (var i = 0; i < steps.length; i++) {
      if (steps[i] * step >= v) return steps[i] * step;
    }
    return 10 * step;
  }

  function linePath(series, pick, x, y) {
    var d = "";
    series.forEach(function (p, i) {
      d += (i === 0 ? "M" : "L") + x(i).toFixed(2) + " " + y(pick(p)).toFixed(2);
    });
    return d;
  }

  // closeToBaseline traces a series left to right, then runs back along the
  // baseline, producing a filled area.
  function closeToBaseline(series, pick, x, y) {
    return linePath(series, pick, x, y) + " " + baselinePath(series, x, y);
  }

  // reversePath walks a series backwards so a stacked band closes on the line
  // beneath it rather than on the baseline.
  function reversePath(series, pick, x, y) {
    var d = "";
    for (var i = series.length - 1; i >= 0; i--) {
      d += "L" + x(i).toFixed(2) + " " + y(pick(series[i])).toFixed(2);
    }
    return d;
  }

  function baselinePath(series, x, y) {
    return "L" + x(series.length - 1).toFixed(2) + " " + y(0).toFixed(2) +
      "L" + x(0).toFixed(2) + " " + y(0).toFixed(2) + "Z";
  }

  // --- wiring ------------------------------------------------------------

  el("connect").addEventListener("click", function () {
    var key = el("key").value.trim();
    if (!key) { el("gate-status").textContent = "Enter the management key."; return; }
    try { localStorage.removeItem(FORGET_STORE); } catch (e) {}
    connect(key);
  });

  el("key").addEventListener("keydown", function (ev) {
    if (ev.key === "Enter") el("connect").click();
  });

  // The key is never written back to storage. It lives in this page's memory
  // for as long as the page is open, so closing the tab ends it. Persisting it
  // would put a management credential in plaintext for any script on this
  // origin, which is more exposure than the feature is worth.
  el("inherit").addEventListener("click", function () {
    var key = panelStoredKey();
    if (!key) { el("gate-status").textContent = "The control panel has no saved key."; return; }
    try { localStorage.removeItem(FORGET_STORE); } catch (e) {}
    connect(key);
  });

  // Forget stops the page and also stops it borrowing the panel's key again on
  // the next load; the panel's own saved session is left untouched.
  el("forget").addEventListener("click", function () {
    try { localStorage.setItem(FORGET_STORE, "1"); } catch (e) {}
    el("key").value = "";
    lockOut("Key forgotten");
  });

  el("toggle-table").addEventListener("click", function () {
    var wrap = el("table-wrap");
    var on = wrap.classList.contains("hidden");
    wrap.classList.toggle("hidden", !on);
    el("toggle-table").textContent = on ? "Hide table" : "Table view";
  });

  var resizeTimer = null;
  window.addEventListener("resize", function () {
    if (resizeTimer) clearTimeout(resizeTimer);
    resizeTimer = setTimeout(function () { drawChart(state.series); }, 120);
  });

  // Nothing is sent until the operator acts. The resource route is
  // unauthenticated, so a page that reached out with a stored credential on
  // load would let any link to this URL spend the panel's key without a
  // deliberate click. The panel's key is offered as a button, never used
  // automatically.
  var forgot = false;
  try { forgot = localStorage.getItem(FORGET_STORE) === "1"; } catch (e) {}

  if (!forgot && panelStoredKey()) {
    el("gate-explain").textContent =
      "The control panel has saved a management key in this browser. Connect with it, " +
      "or enter a different one below. The key is used only for the snapshot request " +
      "to this server and is not written back to storage.";
    el("inherit").textContent = "Use the control panel's saved key";
    show("inherit-row", true);
  } else {
    el("gate-explain").textContent =
      "The control panel has not saved a management key in this browser, so the " +
      "speedometer needs one to read the management API. It is used only for the " +
      "snapshot request to this server and is not written back to storage.";
  }
  show("gate", true);
})();
</script>
</body>
</html>
`
