package admin

// templatesSource holds every admin UI page as Go html/template blocks.
// Kept as a single Go string (rather than embedded files) to avoid adding
// a build-time asset pipeline for what is intentionally a small, plain
// server-rendered UI.
//
// Each top-level page template (login/dashboard/password) begins with the
// shared HTML shell inline and includes the relevant body block — Go's
// html/template doesn't support "extends"-style inheritance, so the shell
// is duplicated per page rather than faked with a fragile workaround.
const templatesSource = `
{{define "style"}}
<style>
  :root { color-scheme: dark; }
  body {
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
    background: #0f1115; color: #e4e6eb; margin: 0; padding: 0;
  }
  .bar {
    display: flex; align-items: center; justify-content: space-between;
    padding: 14px 24px; background: #161922; border-bottom: 1px solid #262b36;
  }
  .bar a { color: #e4e6eb; text-decoration: none; font-weight: 600; }
  .bar nav a { color: #9aa3b2; margin-left: 16px; font-weight: 400; font-size: 14px; }
  .bar nav a:hover { color: #e4e6eb; }
  main { max-width: 720px; margin: 0 auto; padding: 32px 24px; }
  .center { display: flex; align-items: center; justify-content: center; min-height: 90vh; }
  .card {
    background: #161922; border: 1px solid #262b36; border-radius: 10px;
    padding: 28px; width: 100%; max-width: 380px;
  }
  h1 { font-size: 20px; margin: 0 0 4px; }
  h2 { font-size: 15px; color: #9aa3b2; margin: 0 0 20px; font-weight: 400; }
  label { display: block; font-size: 13px; color: #9aa3b2; margin-bottom: 6px; }
  input[type=password], input[type=text] {
    width: 100%; box-sizing: border-box; padding: 10px 12px; margin-bottom: 16px;
    background: #0f1115; border: 1px solid #2c3140; border-radius: 6px;
    color: #e4e6eb; font-size: 14px;
  }
  button {
    width: 100%; padding: 11px; background: #3b82f6; border: none; border-radius: 6px;
    color: #fff; font-size: 14px; font-weight: 600; cursor: pointer;
  }
  button:hover { background: #2f6fdb; }
  .error { background: rgba(239,68,68,0.12); border: 1px solid rgba(239,68,68,0.35);
    color: #f87171; padding: 10px 12px; border-radius: 6px; font-size: 13px; margin-bottom: 16px; }
  .success { background: rgba(34,197,94,0.12); border: 1px solid rgba(34,197,94,0.35);
    color: #4ade80; padding: 10px 12px; border-radius: 6px; font-size: 13px; margin-bottom: 16px; }
  table { width: 100%; border-collapse: collapse; margin-top: 8px; }
  th, td { text-align: left; padding: 8px 10px; border-bottom: 1px solid #262b36; font-size: 13px; }
  th { color: #9aa3b2; font-weight: 500; }
  .group-card { background: #161922; border: 1px solid #262b36; border-radius: 10px; padding: 18px 20px; margin-bottom: 16px; }
  .group-title { font-weight: 600; font-size: 15px; margin-bottom: 2px; }
  .group-meta { color: #9aa3b2; font-size: 12px; margin-bottom: 10px; }
  .empty { color: #9aa3b2; font-size: 14px; text-align: center; padding: 40px 0; }
  .logout-form { margin: 0; }
  .logout-form button { width: auto; padding: 6px 12px; font-size: 13px; background: #262b36; }
  .logout-form button:hover { background: #323847; }
  .health-dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; margin-right: 6px; vertical-align: middle; }
  .health-dot--healthy { background: #4ade80; }
  .health-dot--unhealthy { background: #f87171; }
  .health-dot--unknown { background: #4b5262; }
  .health-label { font-size: 12px; color: #9aa3b2; }
  .refresh-note { color: #565f70; font-size: 11px; margin-bottom: 16px; }
  .badge { display: inline-block; font-size: 11px; padding: 2px 8px; border-radius: 10px; background: #262b36; color: #9aa3b2; }
  .badge--fail { background: rgba(239,68,68,0.15); color: #f87171; }
  .badge--ok { background: rgba(34,197,94,0.15); color: #4ade80; }
  .audit-row td { font-size: 12px; }
  .audit-time { color: #565f70; white-space: nowrap; }
  .algo-form { display: flex; align-items: center; gap: 8px; margin-bottom: 14px; }
  .algo-form select { background: #0f1115; border: 1px solid #2c3140; border-radius: 6px; color: #e4e6eb;
    font-size: 13px; padding: 6px 8px; }
  .algo-form button { width: auto; padding: 6px 12px; font-size: 13px; }
  .row-actions { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
  .row-actions form { display: inline-flex; align-items: center; gap: 4px; margin: 0; }
  .row-actions input[type=number] {
    width: 60px; box-sizing: border-box; padding: 5px 6px; margin: 0;
    background: #0f1115; border: 1px solid #2c3140; border-radius: 6px; color: #e4e6eb; font-size: 12px;
  }
  .row-actions button {
    width: auto; padding: 5px 10px; font-size: 12px; background: #262b36;
  }
  .row-actions button:hover { background: #323847; }
  .row-actions button.danger { background: rgba(239,68,68,0.15); color: #f87171; }
  .row-actions button.danger:hover { background: rgba(239,68,68,0.28); }
  .drained-row { opacity: 0.55; }
  .tag-drained { font-size: 11px; color: #f87171; margin-left: 6px; }
  .tag-override { font-size: 11px; color: #60a5fa; margin-left: 6px; }
</style>
{{end}}

{{define "nav"}}
<div class="bar">
  <a href="/">Go Load Balancer</a>
  <nav style="display:flex; align-items:center;">
    <a href="/">Dashboard</a>
    <a href="/audit">Audit Log</a>
    <a href="/password">Change Password</a>
    <form class="logout-form" method="post" action="/logout" style="margin-left:16px;">
      <button type="submit">Sign out</button>
    </form>
  </nav>
</div>
{{end}}

{{define "login"}}
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Sign in — Go Load Balancer</title>
  {{template "style"}}
</head>
<body>
  <div class="center">
    <div class="card">
      <h1>Sign in</h1>
      <h2>Go Load Balancer — control plane admin</h2>
      {{if .Error}}<div class="error">{{.Error}}</div>{{end}}
      <form method="post" action="/login">
        <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
        <label for="password">Password</label>
        <input type="password" id="password" name="password" autofocus required>
        <button type="submit">Sign in</button>
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
  <title>Dashboard — Go Load Balancer</title>
  {{template "style"}}
</head>
<body>
  {{template "nav"}}
  <main>
    <h1 style="margin-bottom:4px;">Backend groups</h1>
    <p class="refresh-note" id="refresh-note">Auto-refreshing every 5s · last updated just now</p>
    {{if not .Groups}}
      <div class="empty">No backend groups reported yet. The control plane's reconciliation loop runs periodically — check back shortly, or check the container logs if this persists.</div>
    {{else}}
      {{$csrf := .CSRFToken}}
      {{$algorithms := .ValidAlgorithms}}
      {{range .Groups}}
      {{$group := .Group}}
      {{$currentAlgo := .Algorithm}}
      <div class="group-card">
        <div class="group-title">{{.Group}}</div>
        <div class="group-meta">version {{.Version}} · {{len .Backends}} backend(s) · {{.SubscriberCount}} data plane subscriber(s)</div>

        <form class="algo-form" method="post" action="/algorithm">
          <input type="hidden" name="csrf_token" value="{{$csrf}}">
          <input type="hidden" name="group" value="{{$group}}">
          <label for="algorithm-{{$group}}" style="margin:0; color:#9aa3b2; font-size:13px;">Algorithm</label>
          <select id="algorithm-{{$group}}" name="algorithm">
            {{range $algorithms}}
            <option value="{{.}}" {{if eq . $currentAlgo}}selected{{end}}>{{.}}</option>
            {{end}}
          </select>
          <button type="submit">Apply</button>
        </form>

        {{if .Backends}}
        <table>
          <tr><th>Address</th><th>Weight</th><th>Health</th><th>Actions</th></tr>
          {{range .Backends}}
          <tr{{if .Drained}} class="drained-row"{{end}}>
            <td>{{.Address}}{{if .Drained}}<span class="tag-drained">drained</span>{{end}}</td>
            <td>{{.Weight}}{{if .WeightOverridden}}<span class="tag-override">override</span>{{end}}</td>
            <td>
              {{if not .HealthKnown}}
                <span class="health-dot health-dot--unknown"></span><span class="health-label">Unknown</span>
              {{else if .Healthy}}
                <span class="health-dot health-dot--healthy"></span><span class="health-label">Healthy</span>
              {{else}}
                <span class="health-dot health-dot--unhealthy"></span><span class="health-label">Unhealthy</span>
              {{end}}
            </td>
            <td>
              <div class="row-actions">
                <form method="post" action="/override">
                  <input type="hidden" name="csrf_token" value="{{$csrf}}">
                  <input type="hidden" name="group" value="{{$group}}">
                  <input type="hidden" name="address" value="{{.Address}}">
                  <input type="hidden" name="action" value="set_weight">
                  <input type="number" name="weight" min="0" placeholder="wt" value="{{.Weight}}">
                  <button type="submit">Set</button>
                </form>
                <form method="post" action="/override">
                  <input type="hidden" name="csrf_token" value="{{$csrf}}">
                  <input type="hidden" name="group" value="{{$group}}">
                  <input type="hidden" name="address" value="{{.Address}}">
                  {{if .Drained}}
                  <input type="hidden" name="action" value="clear">
                  <button type="submit">Undrain</button>
                  {{else}}
                  <input type="hidden" name="action" value="drain">
                  <button type="submit" class="danger">Drain</button>
                  {{end}}
                </form>
              </div>
            </td>
          </tr>
          {{end}}
        </table>
        {{end}}
      </div>
      {{end}}
    {{end}}
  </main>
  <script>
    // Simple polling auto-refresh: re-fetch the dashboard body every 5s and
    // swap it in, rather than a full page reload, so scroll position and
    // any in-progress interaction aren't disrupted. Falls back gracefully
    // to a plain page (no auto-refresh) if JS is disabled.
    (function () {
      var intervalMs = 5000;
      function refresh() {
        fetch(window.location.pathname, { headers: { 'X-Requested-With': 'dashboard-poll' } })
          .then(function (res) { return res.ok ? res.text() : null; })
          .then(function (html) {
            if (!html) return;
            var parser = new DOMParser();
            var doc = parser.parseFromString(html, 'text/html');
            var newMain = doc.querySelector('main');
            var curMain = document.querySelector('main');
            if (newMain && curMain) curMain.innerHTML = newMain.innerHTML;
            var note = document.getElementById('refresh-note');
            if (note) note.textContent = 'Auto-refreshing every 5s · last updated ' + new Date().toLocaleTimeString();
          })
          .catch(function () { /* network hiccup — try again next tick */ });
      }
      setInterval(refresh, intervalMs);
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
  <title>Audit Log — Go Load Balancer</title>
  {{template "style"}}
</head>
<body>
  {{template "nav"}}
  <main>
    <h1 style="margin-bottom:4px;">Audit log</h1>
    <p class="refresh-note">Most recent {{len .Entries}} event(s), newest first. Kept locally in the container, bounded to the last 500 events.</p>
    {{if not .Entries}}
      <div class="empty">No events recorded yet.</div>
    {{else}}
    <table>
      <tr><th>Time</th><th>Event</th><th>IP</th><th>Details</th></tr>
      {{range .Entries}}
      <tr class="audit-row">
        <td class="audit-time">{{.Time.Format "2006-01-02 15:04:05"}}</td>
        <td>
          {{if eq .Type "login_success"}}<span class="badge badge--ok">Login</span>
          {{else if eq .Type "login_failure"}}<span class="badge badge--fail">Login failed</span>
          {{else if eq .Type "login_rate_limited"}}<span class="badge badge--fail">Rate limited</span>
          {{else if eq .Type "password_changed"}}<span class="badge badge--ok">Password changed</span>
          {{else if eq .Type "password_reset"}}<span class="badge badge--fail">Password reset</span>
          {{else if eq .Type "logout"}}<span class="badge">Logout</span>
          {{else if eq .Type "override_changed"}}<span class="badge">Pool override</span>
          {{else if eq .Type "algorithm_changed"}}<span class="badge">Algorithm</span>
          {{else}}<span class="badge">{{.Type}}</span>
          {{end}}
        </td>
        <td>{{.IP}}</td>
        <td>{{.Message}}</td>
      </tr>
      {{end}}
    </table>
    {{end}}
  </main>
</body>
</html>
{{end}}

{{define "password"}}
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Change Password — Go Load Balancer</title>
  {{template "style"}}
</head>
<body>
  {{template "nav"}}
  <main>
    <div style="max-width:380px; margin:0 auto;">
      <h1>Change password</h1>
      <h2>Changing your password signs out all other active sessions.</h2>
      {{if .Error}}<div class="error">{{.Error}}</div>{{end}}
      {{if .Success}}<div class="success">Password updated successfully.</div>{{end}}
      <form method="post" action="/password">
        <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
        <label for="current_password">Current password</label>
        <input type="password" id="current_password" name="current_password" required>
        <label for="new_password">New password</label>
        <input type="password" id="new_password" name="new_password" required minlength="12">
        <label for="confirm_password">Confirm new password</label>
        <input type="password" id="confirm_password" name="confirm_password" required minlength="12">
        <button type="submit">Update password</button>
      </form>
    </div>
  </main>
</body>
</html>
{{end}}
`
