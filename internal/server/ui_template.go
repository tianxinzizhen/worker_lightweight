package server

import (
	"html/template"
)

// uiTmpl is the single template powering the dashboard. It is parsed once at
// init so request handling only does Execute — no per-request parse cost.
//
// Template actions use the {{range}}, {{.Field}} syntax. JS auto-refreshes
// the page so the dashboard feels live without a websocket.
var uiTmpl = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Task Scheduler Dashboard</title>
<style>
  body { font-family: -apple-system, Segoe UI, Roboto, sans-serif; margin: 0; background:#f6f7f9; color:#222; }
  header { background:#1f2937; color:#fff; padding:16px 24px; }
  header h1 { margin:0; font-size:20px; font-weight:600; }
  header span { opacity:.7; font-size:13px; margin-left:8px; }
  main { max-width:1200px; margin:24px auto; padding:0 16px; }
  .stat { display:inline-block; background:#fff; border:1px solid #e5e7eb; border-radius:8px; padding:8px 14px; margin-right:12px; font-size:13px; }
  .stat b { display:block; font-size:20px; }
  table { width:100%; border-collapse:collapse; background:#fff; border-radius:8px; overflow:hidden; box-shadow:0 1px 2px rgba(0,0,0,.04); }
  th, td { padding:10px 12px; text-align:left; border-bottom:1px solid #f1f1f1; font-size:13px; }
  th { background:#f9fafb; font-weight:600; color:#374151; }
  td.cmd { font-family:monospace; }
  .badge { padding:2px 8px; border-radius:10px; font-size:11px; font-weight:600; }
  .pending { background:#fef3c7; color:#92400e; }
  .running { background:#dbeafe; color:#1e40af; }
  .success { background:#d1fae5; color:#065f46; }
  .failed, .dead { background:#fee2e2; color:#991b1b; }
  .empty { padding:40px; text-align:center; color:#9ca3af; }
</style>
</head>
<body>
<header>
  <h1>Task Scheduler <span>{{.Server}} · {{.Now}}</span></h1>
</header>
<main>
  <div style="margin-bottom:16px">
    <span class="stat"><b>{{len .Tasks}}</b>shown (latest 100)</span>
    <span class="stat"><b>{{.Server}}</b>endpoint</span>
  </div>
  {{if .Tasks}}
  <table>
    <thead>
      <tr><th>ID</th><th>Name</th><th>Command</th><th>Type</th><th>Status</th><th>Worker</th><th>Result</th><th>Updated</th></tr>
    </thead>
    <tbody>
    {{range .Tasks}}
      <tr>
        <td>{{.ID}}</td>
        <td>{{.Name}}</td>
        <td class="cmd">{{.Command}}</td>
        <td>{{.Type}}</td>
        <td><span class="badge {{.Status}}">{{.Status}}</span></td>
        <td>{{.Worker}}</td>
        <td>{{.Result}}</td>
        <td>{{.Updated}}</td>
      </tr>
    {{end}}
    </tbody>
  </table>
  {{else}}
  <div class="empty">No tasks yet. POST /api/tasks to create one.</div>
  {{end}}
</main>
<script>setTimeout(function(){location.reload();},3000);</script>
</body>
</html>`))
