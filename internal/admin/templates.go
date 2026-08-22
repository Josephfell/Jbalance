package admin

// templatesSource holds every admin UI page as Go html/template blocks.
// Kept as a single Go string (rather than embedded files) to avoid adding
// a build-time asset pipeline for what is intentionally a small, plain
// server-rendered UI — no JS framework, no external fonts/icons/CDNs, so
// the admin UI keeps working with no network access beyond the browser
// talking to this server.
//
// Visual design follows the "jbalance console" mockup (dark, indigo
// accent, sidebar nav) but only surfaces data the control plane actually
// tracks — the mockup's Routes/TLS/Config screens depict features that
// don't exist yet in this codebase and are intentionally not included
// here (see the roadmap discussion for what it would take to add them).
//
// Each top-level page template (login/dashboard/fleet/audit/password)
// begins with the shared HTML shell inline and includes the "style" and
// "sidebar" blocks — Go's html/template doesn't support "extends"-style
// inheritance, so the shell is duplicated per page rather than faked with
// a fragile workaround.
const templatesSource = `
{{define "style"}}
<style>
  :root {
    color-scheme: dark;
    --bg: #14151f;
    --surface: #1b1d2b;
    --surface-soft: color-mix(in srgb, #1b1d2b 55%, transparent);
    --border: #2a2d3f;
    --border-soft: color-mix(in srgb, #2a2d3f 65%, transparent);
    --text: #e7e7ee;
    --text-muted: #9195ab;
    --text-faint: #6b6f85;
    --accent: #9184d9;
    --accent-weak: color-mix(in srgb, #9184d9 14%, transparent);
    --accent-weaker: color-mix(in srgb, #9184d9 8%, transparent);
    --ok: #4ade80;
    --warn: #facc15;
    --down: #f87171;
    --unknown: #6b6f85;
    --radius: 8px;
    --radius-sm: 5px;
    --mono: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
    background: var(--bg); color: var(--text); font-size: 13.5px; line-height: 1.5;
  }
  a { color: var(--accent); text-decoration: none; }
  h1, h2, h3 { font-weight: 600; letter-spacing: -0.01em; margin: 0; }

  .app { display: flex; min-height: 100vh; }

  /* — sidebar — */
  .sidebar {
    width: 208px; flex: none; display: flex; flex-direction: column;
    border-right: 1px solid var(--border); background: color-mix(in srgb, var(--surface) 55%, var(--bg));
    padding: 16px 0;
  }
  .brand { display: flex; align-items: center; gap: 9px; padding: 0 16px 16px; }
  .brand-mark {
    width: 24px; height: 24px; border-radius: 7px; border: 1px solid var(--accent);
    display: flex; align-items: center; justify-content: center; flex: none; color: var(--accent);
  }
  .brand-name { font-weight: 600; font-size: 14.5px; letter-spacing: -0.01em; }
  .brand-sub { font-size: 10px; color: var(--text-faint); letter-spacing: .06em; text-transform: uppercase; }
  .side-nav { display: flex; flex-direction: column; gap: 2px; padding: 4px 8px; }
  .side-nav a {
    display: flex; align-items: center; gap: 9px; padding: 8px 10px; border-radius: var(--radius-sm);
    color: var(--text-muted); font-size: 13px; font-weight: 500; border: 1px solid transparent;
  }
  .side-nav a svg { flex: none; width: 15px; height: 15px; }
  .side-nav a:hover { background: color-mix(in srgb, var(--text) 6%, transparent); color: var(--text); }
  .side-nav a.active {
    color: var(--accent); background: var(--accent-weak);
    border-color: color-mix(in srgb, var(--accent) 45%, transparent);
  }
  .side-foot {
    margin-top: auto; padding: 14px 16px 0; border-top: 1px solid var(--border);
    display: flex; flex-direction: column; gap: 6px;
  }
  .side-foot .status { display: flex; align-items: center; gap: 7px; font-size: 11px; color: var(--text-muted); }
  .dot { width: 6px; height: 6px; border-radius: 50%; flex: none; display: inline-block; }
  .dot--ok { background: var(--ok); animation: pulse 2.2s ease-in-out infinite; }
  .dot--down { background: var(--down); }
  .dot--unknown { background: var(--unknown); }
  @keyframes pulse { 0%,100% { opacity: 1; } 50% { opacity: .45; } }
  .side-foot form { margin: 0; }
  .signout-btn {
    width: 100%; text-align: left; padding: 8px 10px; border-radius: var(--radius-sm);
    border: 1px solid transparent; background: transparent; color: var(--text-muted);
    font: 500 13px inherit; cursor: pointer; display: flex; align-items: center; gap: 9px;
  }
  .signout-btn:hover { background: color-mix(in srgb, var(--down) 10%, transparent); color: var(--down); }
  .signout-btn svg { width: 15px; height: 15px; flex: none; }

  /* — main / header — */
  .main { flex: 1; min-width: 0; display: flex; flex-direction: column; }
  .topbar {
    height: 54px; flex: none; display: flex; align-items: baseline; gap: 10px;
    padding: 0 22px; border-bottom: 1px solid var(--border);
  }
  .topbar h1 { font-size: 15.5px; }
  .topbar .sub { font-size: 11.5px; color: var(--text-faint); }
  .topbar .right { margin-left: auto; display: flex; align-items: center; gap: 8px; align-self: center; }
  .clock-pill {
    display: flex; align-items: center; gap: 6px; padding: 4px 10px; border: 1px solid var(--border);
    border-radius: var(--radius); font-size: 11px; color: var(--text-muted); font-family: var(--mono);
  }
  .content { flex: 1; padding: 20px 22px 30px; display: flex; flex-direction: column; gap: 14px; overflow: auto; }
  .refresh-note { font-size: 11px; color: var(--text-faint); margin: -4px 0 0; }

  /* — cards / sections — */
  .card {
    border: 1px solid var(--border); border-radius: var(--radius); background: var(--surface-soft);
  }
  .card-head {
    display: flex; align-items: center; gap: 10px; padding: 11px 14px; border-bottom: 1px solid var(--border);
  }
  .card-head h2 { font-size: 13px; }
  .card-head .meta { font-size: 11px; color: var(--text-faint); }
  .card-pad { padding: 14px; }
  .stat-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 10px; }
  .stat {
    border: 1px solid var(--border); border-radius: var(--radius); padding: 11px 13px;
    background: var(--surface-soft); display: flex; flex-direction: column; gap: 5px;
  }
  .stat-label { font-size: 10.5px; letter-spacing: .06em; text-transform: uppercase; color: var(--text-faint); }
  .stat-value { font-size: 22px; font-weight: 600; letter-spacing: -0.02em; }
  .stat-note { font-size: 11px; color: var(--text-muted); }

  /* — tables — */
  .table { width: 100%; border-collapse: collapse; font-size: 13px; }
  .table th {
    text-align: left; font-size: 10.5px; letter-spacing: .06em; text-transform: uppercase;
    color: var(--text-faint); font-weight: 500; padding: 8px 14px; border-bottom: 1px solid var(--border);
  }
  .table td { padding: 9px 14px; border-top: 1px solid var(--border-soft); vertical-align: middle; }
  .table tr:hover td { background: color-mix(in srgb, var(--accent) 5%, transparent); }
  .table .mono { font-family: var(--mono); font-size: 12.5px; }
  .table .muted { color: var(--text-faint); font-size: 11.5px; }
  .drained-row td { opacity: .5; }

  /* — pills / tags — */
  .pill {
    display: inline-flex; align-items: center; gap: 5px; font-size: 11px; padding: 2px 9px;
    border-radius: 10px; border: 1px solid transparent; line-height: 1.6;
  }
  .pill--ok { color: var(--ok); border-color: var(--ok); }
  .pill--down { color: var(--down); border-color: var(--down); }
  .pill--unknown { color: var(--unknown); border-color: var(--unknown); }
  .tag { font-size: 10.5px; margin-left: 6px; padding: 1px 7px; border-radius: 8px; }
  .tag--drained { color: var(--down); border: 1px solid color-mix(in srgb, var(--down) 55%, transparent); }
  .tag--override { color: var(--accent); border: 1px solid color-mix(in srgb, var(--accent) 55%, transparent); }

  /* — forms / buttons — */
  .btn {
    display: inline-flex; align-items: center; justify-content: center; gap: 6px; cursor: pointer;
    font: 500 12.5px inherit; padding: 6px 12px; border-radius: var(--radius-sm);
    border: 1px solid var(--border); background: transparent; color: var(--text);
  }
  .btn:hover { border-color: var(--text-muted); }
  .btn-primary { border-color: var(--accent); color: var(--accent); }
  .btn-primary:hover { background: var(--accent-weak); }
  .btn-danger { border-color: color-mix(in srgb, var(--down) 55%, transparent); color: var(--down); }
  .btn-danger:hover { background: color-mix(in srgb, var(--down) 12%, transparent); }
  .btn-full { width: 100%; padding: 10px; font-size: 13.5px; }
  .btn[disabled] { opacity: .45; cursor: not-allowed; }
  .row-actions { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
  .row-actions form { display: inline-flex; align-items: center; gap: 4px; margin: 0; }
  input[type=password], input[type=text], input[type=number], select {
    font: inherit; font-size: 13px; color: var(--text); background: var(--bg);
    border: 1px solid var(--border); border-radius: var(--radius-sm); padding: 7px 9px;
  }
  input:hover, select:hover { border-color: var(--text-muted); }
  input:focus-visible, select:focus-visible { outline: 1.5px solid var(--accent); outline-offset: 0; }
  .weight-input { width: 56px; padding: 5px 7px; font-size: 12px; }
  .field { display: flex; flex-direction: column; gap: 5px; margin-bottom: 14px; }
  .field label { font-size: 11.5px; color: var(--text-muted); }
  .algo-select { font-size: 12.5px; padding: 6px 8px; }

  .error {
    background: color-mix(in srgb, var(--down) 12%, transparent); border: 1px solid color-mix(in srgb, var(--down) 35%, transparent);
    color: var(--down); padding: 10px 12px; border-radius: var(--radius); font-size: 13px; margin-bottom: 14px;
  }
  .success {
    background: color-mix(in srgb, var(--ok) 12%, transparent); border: 1px solid color-mix(in srgb, var(--ok) 35%, transparent);
    color: var(--ok); padding: 10px 12px; border-radius: var(--radius); font-size: 13px; margin-bottom: 14px;
  }
  .empty { color: var(--text-faint); font-size: 13.5px; text-align: center; padding: 46px 0; }

  /* — login — */
  .center-page { display: flex; align-items: center; justify-content: center; min-height: 100vh; padding: 20px; }
  .login-card { width: 100%; max-width: 380px; border: 1px solid var(--border); border-radius: 12px; background: var(--surface); padding: 30px; }

  /* — routes editor — */
  .routes-table input[type=text], .routes-table select {
    width: 100%; font-size: 12.5px; padding: 6px 8px;
  }
  .routes-table td { padding: 6px 8px; vertical-align: top; }
  .routes-table th { padding: 8px; }
  .routes-order { width: 34px; text-align: center; color: var(--text-faint); font-family: var(--mono); font-size: 12px; padding-top: 10px; }
  .routes-note { font-size: 11.5px; color: var(--text-faint); margin: 0 0 12px; }
  .routes-actions { display: flex; justify-content: space-between; align-items: center; padding: 11px 14px; border-top: 1px solid var(--border); }
  .col-host { width: 15%; } .col-path { width: 15%; } .col-methods { width: 15%; }
  .col-group { width: 15%; } .col-name { width: 20%; } .col-del { width: 60px; text-align: center; }
  .login-brand { display: flex; align-items: center; gap: 10px; margin-bottom: 20px; }
  .login-title { font-size: 19px; margin-bottom: 3px; }
  .login-sub { font-size: 13px; color: var(--text-muted); margin-bottom: 20px; }
</style>
{{end}}

{{define "sidebar"}}
<aside class="sidebar">
  <div class="brand">
    <div class="brand-mark">
      <svg viewBox="0 0 24 24" fill="none" width="14" height="14"><path d="M4 7h6l2 2h8M4 17h6l2-2h8M17 4l3 3-3 3M17 14l3 3-3 3" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>
    </div>
    <div>
      <div class="brand-name">jbalance</div>
      <div class="brand-sub">control plane</div>
    </div>
  </div>
  <nav class="side-nav">
    <a href="/" {{if eq .Active "dashboard"}}class="active"{{end}}>
      <svg viewBox="0 0 24 24" fill="none"><rect x="3" y="3" width="8" height="8" rx="1.5" stroke="currentColor" stroke-width="1.6"/><rect x="13" y="3" width="8" height="5" rx="1.5" stroke="currentColor" stroke-width="1.6"/><rect x="13" y="10" width="8" height="11" rx="1.5" stroke="currentColor" stroke-width="1.6"/><rect x="3" y="13" width="8" height="8" rx="1.5" stroke="currentColor" stroke-width="1.6"/></svg>
      <span>Dashboard</span>
    </a>
    <a href="/routes" {{if eq .Active "routes"}}class="active"{{end}}>
      <svg viewBox="0 0 24 24" fill="none"><path d="M4 6h6l2 2h8M4 18h6l2-2h8" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>
      <span>Routes</span>
    </a>
    <a href="/fleet" {{if eq .Active "fleet"}}class="active"{{end}}>
      <svg viewBox="0 0 24 24" fill="none"><rect x="3" y="4" width="18" height="5" rx="1.3" stroke="currentColor" stroke-width="1.6"/><rect x="3" y="10.5" width="18" height="5" rx="1.3" stroke="currentColor" stroke-width="1.6"/><rect x="3" y="17" width="18" height="4" rx="1.3" stroke="currentColor" stroke-width="1.6"/></svg>
      <span>Fleet</span>
    </a>
    <a href="/audit" {{if eq .Active "audit"}}class="active"{{end}}>
      <svg viewBox="0 0 24 24" fill="none"><circle cx="12" cy="12" r="8.5" stroke="currentColor" stroke-width="1.6"/><path d="M12 7.5V12l3 2" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>
      <span>Audit Log</span>
    </a>
    <a href="/password" {{if eq .Active "password"}}class="active"{{end}}>
      <svg viewBox="0 0 24 24" fill="none"><circle cx="8" cy="15" r="4" stroke="currentColor" stroke-width="1.6"/><path d="M11.2 12 19 4.2M15.5 7.7l2.3 2.3M18 5.2l2.3 2.3" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>
      <span>Change Password</span>
    </a>
  </nav>
  <div class="side-foot">
    <div class="status"><span class="dot dot--ok"></span>control plane process running</div>
    <form method="post" action="/logout">
      <button type="submit" class="signout-btn">
        <svg viewBox="0 0 24 24" fill="none"><path d="M15 4H8a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h7" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/><path d="M10 12h11m0 0-3-3m3 3-3 3" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>
        Sign out
      </button>
    </form>
  </div>
</aside>
{{end}}

{{define "login"}}
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Sign in — jbalance</title>
  {{template "style"}}
</head>
<body>
  <div class="center-page">
    <div class="login-card">
      <div class="login-brand">
        <div class="brand-mark">
          <svg viewBox="0 0 24 24" fill="none" width="14" height="14"><path d="M4 7h6l2 2h8M4 17h6l2-2h8M17 4l3 3-3 3M17 14l3 3-3 3" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round"/></svg>
        </div>
        <div class="brand-name">jbalance</div>
      </div>
      <div class="login-title">Sign in</div>
      <div class="login-sub">Control plane admin console</div>
      {{if .Error}}<div class="error">{{.Error}}</div>{{end}}
      <form method="post" action="/login">
        <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
        <div class="field">
          <label for="password">Password</label>
          <input type="password" id="password" name="password" autofocus required>
        </div>
        <button type="submit" class="btn btn-primary btn-full">Sign in</button>
      </form>
    </div>
  </div>
</body>
</html>
{{end}}

{{define "dashboard"}}
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Dashboard — jbalance</title>
  {{template "style"}}
</head>
<body>
  <div class="app">
    {{template "sidebar" (dict "Active" "dashboard")}}
    <div class="main">
      <header class="topbar">
        <h1>Dashboard</h1>
        <span class="sub">backend groups &amp; health, as reported by the pool provider and subscribed data planes</span>
        <div class="right"><span class="clock-pill" id="clock"></span></div>
      </header>
      <div class="content">
        <p class="refresh-note" id="refresh-note">Auto-refreshing every 5s · last updated just now</p>
        {{if not .Groups}}
          <div class="empty">No backend groups reported yet. The control plane's reconciliation loop runs periodically — check back shortly, or check the container logs if this persists.</div>
        {{else}}
          {{$csrf := .CSRFToken}}
          {{$algorithms := .ValidAlgorithms}}
          {{range .Groups}}
          {{$group := .Group}}
          {{$currentAlgo := .Algorithm}}
          <section class="card">
            <div class="card-head">
              <h2>{{.Group}}</h2>
              <span class="meta">version {{.Version}} · {{len .Backends}} backend(s) · {{.SubscriberCount}} data plane subscriber(s)</span>
              <form method="post" action="/algorithm" style="margin-left:auto;display:flex;align-items:center;gap:7px">
                <input type="hidden" name="csrf_token" value="{{$csrf}}">
                <input type="hidden" name="group" value="{{$group}}">
                <select name="algorithm" class="algo-select" aria-label="Load-balancing algorithm for {{$group}}">
                  {{range $algorithms}}
                  <option value="{{.}}" {{if eq . $currentAlgo}}selected{{end}}>{{.}}</option>
                  {{end}}
                </select>
                <button type="submit" class="btn">Apply</button>
              </form>
            </div>
            {{if .Backends}}
            <table class="table">
              <tr><th>Address</th><th>Weight</th><th>Health</th><th>Actions</th></tr>
              {{range .Backends}}
              <tr{{if .Drained}} class="drained-row"{{end}}>
                <td class="mono">{{.Address}}{{if .Drained}}<span class="tag tag--drained">drained</span>{{end}}</td>
                <td class="mono">{{.Weight}}{{if .WeightOverridden}}<span class="tag tag--override">override</span>{{end}}</td>
                <td>
                  {{if not .HealthKnown}}
                    <span class="pill pill--unknown"><span class="dot dot--unknown"></span>Unknown</span>
                  {{else if .Healthy}}
                    <span class="pill pill--ok"><span class="dot dot--ok"></span>Healthy</span>
                  {{else}}
                    <span class="pill pill--down"><span class="dot dot--down"></span>Unhealthy</span>
                  {{end}}
                </td>
                <td>
                  <div class="row-actions">
                    <form method="post" action="/override">
                      <input type="hidden" name="csrf_token" value="{{$csrf}}">
                      <input type="hidden" name="group" value="{{$group}}">
                      <input type="hidden" name="address" value="{{.Address}}">
                      <input type="hidden" name="action" value="set_weight">
                      <input type="number" name="weight" min="0" placeholder="wt" value="{{.Weight}}" class="weight-input">
                      <button type="submit" class="btn">Set</button>
                    </form>
                    <form method="post" action="/override">
                      <input type="hidden" name="csrf_token" value="{{$csrf}}">
                      <input type="hidden" name="group" value="{{$group}}">
                      <input type="hidden" name="address" value="{{.Address}}">
                      {{if .Drained}}
                      <input type="hidden" name="action" value="clear">
                      <button type="submit" class="btn">Undrain</button>
                      {{else}}
                      <input type="hidden" name="action" value="drain">
                      <button type="submit" class="btn btn-danger">Drain</button>
                      {{end}}
                    </form>
                  </div>
                </td>
              </tr>
              {{end}}
            </table>
            {{end}}
          </section>
          {{end}}
        {{end}}
      </div>
    </div>
  </div>
  <script>
    // Live clock — purely cosmetic, computed client-side so it's always
    // accurate without a server round trip.
    (function () {
      var el = document.getElementById('clock');
      function tick() { if (el) el.textContent = 'live · ' + new Date().toLocaleTimeString(); }
      tick(); setInterval(tick, 1000);
    })();
    // Auto-refresh: re-fetch this page's content every 5s and swap the
    // group cards in, rather than a full reload, so in-progress form
    // input isn't disrupted. Falls back to a static page if JS is off.
    (function () {
      function refresh() {
        fetch(window.location.pathname, { headers: { 'X-Requested-With': 'dashboard-poll' } })
          .then(function (res) { return res.ok ? res.text() : null; })
          .then(function (html) {
            if (!html) return;
            var doc = new DOMParser().parseFromString(html, 'text/html');
            var newContent = doc.querySelector('.content');
            var curContent = document.querySelector('.content');
            if (newContent && curContent) curContent.innerHTML = newContent.innerHTML;
            var note = document.getElementById('refresh-note');
            if (note) note.textContent = 'Auto-refreshing every 5s · last updated ' + new Date().toLocaleTimeString();
          })
          .catch(function () { /* network hiccup — try again next tick */ });
      }
      setInterval(refresh, 5000);
    })();
  </script>
</body>
</html>
{{end}}

{{define "fleet"}}
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Fleet — jbalance</title>
  {{template "style"}}
</head>
<body>
  <div class="app">
    {{template "sidebar" (dict "Active" "fleet")}}
    <div class="main">
      <header class="topbar">
        <h1>Fleet</h1>
        <span class="sub">data plane instances that have connected to this control plane</span>
        <div class="right"><span class="clock-pill" id="clock"></span></div>
      </header>
      <div class="content">
        {{if not .Instances}}
          <div class="empty">No data plane instances have connected yet. An instance appears here as soon as it opens its gRPC stream to this control plane.</div>
        {{else}}
        <section class="card">
          <table class="table">
            <tr><th>Instance</th><th>Group</th><th>Stream</th><th>Connected</th><th>Last health report</th></tr>
            {{range .Instances}}
            <tr>
              <td class="mono">{{.InstanceID}}</td>
              <td class="mono">{{.Group}}</td>
              <td>
                {{if .Connected}}
                  <span class="pill pill--ok"><span class="dot dot--ok"></span>Streaming</span>
                {{else}}
                  <span class="pill pill--unknown"><span class="dot dot--unknown"></span>Disconnected</span>
                {{end}}
              </td>
              <td class="muted">{{.ConnectedFor}}</td>
              <td class="muted">
                {{if .HasHealthReport}}
                  {{.LastHealthReport}} · {{.ReportedBackends}} backend(s)
                {{else}}
                  no report yet
                {{end}}
              </td>
            </tr>
            {{end}}
          </table>
        </section>
        {{end}}
      </div>
    </div>
  </div>
  <script>
    (function () {
      var el = document.getElementById('clock');
      function tick() { if (el) el.textContent = 'live · ' + new Date().toLocaleTimeString(); }
      tick(); setInterval(tick, 1000);
    })();
  </script>
</body>
</html>
{{end}}

{{define "routes"}}
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Routes — jbalance</title>
  {{template "style"}}
</head>
<body>
  <div class="app">
    {{template "sidebar" (dict "Active" "routes")}}
    <div class="main">
      <header class="topbar">
        <h1>L7 Routes</h1>
        <span class="sub">evaluated top to bottom · first match wins · applies to every data plane instance</span>
        <div class="right"><span class="clock-pill" id="clock"></span></div>
      </header>
      <div class="content">
        <p class="routes-note">A request matching no rule below (or an empty table) falls back to each data plane instance's own <code>-group</code> flag. Host and path-prefix fields may be left blank to match anything; the methods field accepts a comma-separated list (e.g. <code>GET, POST</code>) and is left blank to match any method.</p>
        <form method="post" action="/routes" id="routes-form">
          <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
          <section class="card">
            <table class="table routes-table">
              <tr>
                <th></th>
                <th class="col-name">Name</th>
                <th class="col-host">Host</th>
                <th class="col-path">Path prefix</th>
                <th class="col-methods">Methods</th>
                <th class="col-group">Target group</th>
                <th class="col-del">Remove</th>
              </tr>
              <tbody id="routes-body">
                {{range .Rows}}
                <tr>
                  <td class="routes-order">{{.Order}}</td>
                  <td><input type="text" name="name" value="{{.Name}}" placeholder="optional"></td>
                  <td><input type="text" name="host" value="{{.Host}}" placeholder="* (any)"></td>
                  <td><input type="text" name="path_prefix" value="{{.PathPrefix}}" placeholder="/"></td>
                  <td><input type="text" name="methods" value="{{.Methods}}" placeholder="any"></td>
                  <td>
                    <select name="target_group">
                      <option value="">— select —</option>
                      {{$current := .TargetGroup}}
                      {{range $.Groups}}
                      <option value="{{.}}" {{if eq . $current}}selected{{end}}>{{.}}</option>
                      {{end}}
                    </select>
                  </td>
                  <td class="col-del">
                    <input type="hidden" name="order" value="{{.Order}}">
                    <input type="hidden" name="action" value="keep">
                    <button type="button" class="btn btn-danger route-remove-btn">Remove</button>
                  </td>
                </tr>
                {{end}}
              </tbody>
            </table>
            <div class="routes-actions">
              <button type="button" class="btn" id="add-route-btn">+ Add rule</button>
              <button type="submit" class="btn btn-primary">Save route table</button>
            </div>
          </section>
        </form>
      </div>
    </div>
  </div>
  <script>
    (function () {
      var el = document.getElementById('clock');
      function tick() { if (el) el.textContent = 'live · ' + new Date().toLocaleTimeString(); }
      tick(); setInterval(tick, 1000);
    })();
  </script>
  <script>
    // Client-side row add/remove for a JS-free-degradable form: every row
    // is plain inputs the server already knows how to parse (order/host/
    // path_prefix/methods/target_group/name/action), so this script only
    // manipulates the DOM — the actual save is a normal form POST with no
    // fetch() involved. Without JS, the "+ Add rule"/"Remove" buttons are
    // simply inert; existing rows can still be edited and saved.
    (function () {
      var groupOptions = {{.GroupsJSON}};
      var body = document.getElementById('routes-body');
      var nextOrder = {{.NextOrder}};

      function renumber() {
        Array.prototype.forEach.call(body.querySelectorAll('tr'), function (tr, i) {
          tr.querySelector('.routes-order').textContent = i;
          tr.querySelector('input[name="order"]').value = i;
        });
      }

      function buildGroupSelect() {
        var select = document.createElement('select');
        select.name = 'target_group';
        var blank = document.createElement('option');
        blank.value = ''; blank.textContent = '— select —';
        select.appendChild(blank);
        groupOptions.forEach(function (g) {
          var opt = document.createElement('option');
          opt.value = g; opt.textContent = g;
          select.appendChild(opt);
        });
        return select;
      }

      function addRow() {
        var tr = document.createElement('tr');

        var orderTd = document.createElement('td');
        orderTd.className = 'routes-order';
        orderTd.textContent = String(nextOrder);
        tr.appendChild(orderTd);

        [['name', 'optional'], ['host', '* (any)'], ['path_prefix', '/'], ['methods', 'any']].forEach(function (f) {
          var td = document.createElement('td');
          var input = document.createElement('input');
          input.type = 'text'; input.name = f[0]; input.placeholder = f[1];
          td.appendChild(input);
          tr.appendChild(td);
        });

        var groupTd = document.createElement('td');
        groupTd.appendChild(buildGroupSelect());
        tr.appendChild(groupTd);

        var delTd = document.createElement('td');
        delTd.className = 'col-del';
        var orderInput = document.createElement('input');
        orderInput.type = 'hidden'; orderInput.name = 'order'; orderInput.value = String(nextOrder);
        var actionInput = document.createElement('input');
        actionInput.type = 'hidden'; actionInput.name = 'action'; actionInput.value = 'keep';
        var removeBtn = document.createElement('button');
        removeBtn.type = 'button'; removeBtn.className = 'btn btn-danger route-remove-btn';
        removeBtn.textContent = 'Remove';
        delTd.appendChild(orderInput); delTd.appendChild(actionInput); delTd.appendChild(removeBtn);
        tr.appendChild(delTd);

        body.appendChild(tr);
        nextOrder++;
      }

      document.getElementById('add-route-btn').addEventListener('click', addRow);
      body.addEventListener('click', function (e) {
        if (e.target.classList.contains('route-remove-btn')) {
          e.target.closest('tr').remove();
          renumber();
        }
      });
    })();
  </script>
</body>
</html>
{{end}}

{{define "audit"}}
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Audit Log — jbalance</title>
  {{template "style"}}
</head>
<body>
  <div class="app">
    {{template "sidebar" (dict "Active" "audit")}}
    <div class="main">
      <header class="topbar">
        <h1>Audit Log</h1>
        <span class="sub">most recent {{len .Entries}} event(s), newest first · bounded to the last 500, kept locally</span>
        <div class="right"><span class="clock-pill" id="clock"></span></div>
      </header>
      <div class="content">
        {{if not .Entries}}
          <div class="empty">No events recorded yet.</div>
        {{else}}
        <section class="card">
          <table class="table">
            <tr><th>Time</th><th>Event</th><th>IP</th><th>Details</th></tr>
            {{range .Entries}}
            <tr>
              <td class="mono muted">{{.Time.Format "2006-01-02 15:04:05"}}</td>
              <td>
                {{if eq .Type "login_success"}}<span class="pill pill--ok">Login</span>
                {{else if eq .Type "login_failure"}}<span class="pill pill--down">Login failed</span>
                {{else if eq .Type "login_rate_limited"}}<span class="pill pill--down">Rate limited</span>
                {{else if eq .Type "password_changed"}}<span class="pill pill--ok">Password changed</span>
                {{else if eq .Type "password_reset"}}<span class="pill pill--down">Password reset</span>
                {{else if eq .Type "logout"}}<span class="pill pill--unknown">Logout</span>
                {{else if eq .Type "override_changed"}}<span class="pill pill--unknown">Pool override</span>
                {{else if eq .Type "algorithm_changed"}}<span class="pill pill--unknown">Algorithm</span>
                {{else}}<span class="pill pill--unknown">{{.Type}}</span>
                {{end}}
              </td>
              <td class="mono muted">{{.IP}}</td>
              <td>{{.Message}}</td>
            </tr>
            {{end}}
          </table>
        </section>
        {{end}}
      </div>
    </div>
  </div>
  <script>
    (function () {
      var el = document.getElementById('clock');
      function tick() { if (el) el.textContent = 'live · ' + new Date().toLocaleTimeString(); }
      tick(); setInterval(tick, 1000);
    })();
  </script>
</body>
</html>
{{end}}

{{define "password"}}
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Change Password — jbalance</title>
  {{template "style"}}
</head>
<body>
  <div class="app">
    {{template "sidebar" (dict "Active" "password")}}
    <div class="main">
      <header class="topbar">
        <h1>Change Password</h1>
        <span class="sub">changing your password signs out every other active session</span>
        <div class="right"><span class="clock-pill" id="clock"></span></div>
      </header>
      <div class="content">
        <section class="card card-pad" style="max-width:380px">
          {{if .Error}}<div class="error">{{.Error}}</div>{{end}}
          {{if .Success}}<div class="success">Password updated successfully.</div>{{end}}
          <form method="post" action="/password">
            <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
            <div class="field">
              <label for="current_password">Current password</label>
              <input type="password" id="current_password" name="current_password" required>
            </div>
            <div class="field">
              <label for="new_password">New password</label>
              <input type="password" id="new_password" name="new_password" required minlength="12">
            </div>
            <div class="field">
              <label for="confirm_password">Confirm new password</label>
              <input type="password" id="confirm_password" name="confirm_password" required minlength="12">
            </div>
            <button type="submit" class="btn btn-primary btn-full">Update password</button>
          </form>
        </section>
      </div>
    </div>
  </div>
  <script>
    (function () {
      var el = document.getElementById('clock');
      function tick() { if (el) el.textContent = 'live · ' + new Date().toLocaleTimeString(); }
      tick(); setInterval(tick, 1000);
    })();
  </script>
</body>
</html>
{{end}}
`
