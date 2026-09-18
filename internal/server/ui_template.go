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
  header { background:#1f2937; color:#fff; padding:16px 24px; display:flex; justify-content:space-between; align-items:center; }
  header h1 { margin:0; font-size:20px; font-weight:600; }
  header h1 span { opacity:.7; font-size:13px; margin-left:8px; }
  header nav a { color:#9ca3af; text-decoration:none; margin-left:16px; font-size:13px; }
  header nav a:hover { color:#fff; }
  header nav a.active { color:#fff; }
  main { max-width:1200px; margin:24px auto; padding:0 16px; }
  .stat { display:inline-block; background:#fff; border:1px solid #e5e7eb; border-radius:8px; padding:8px 14px; margin-right:12px; font-size:13px; }
  .stat b { display:block; font-size:20px; }
  table { width:100%; border-collapse:collapse; background:#fff; border-radius:8px; overflow:hidden; box-shadow:0 1px 2px rgba(0,0,0,.04); table-layout:fixed; }
  th, td { padding:10px 12px; text-align:left; border-bottom:1px solid #f1f1f1; font-size:13px; vertical-align:middle; }
  th { background:#f9fafb; font-weight:600; color:#374151; }
  th:nth-child(1) { width:55px; }
  th:nth-child(2) { width:130px; }
  th:nth-child(3) { width:160px; }
  th:nth-child(4) { width:55px; }
  th:nth-child(5) { width:75px; }
  th:nth-child(6) { width:60px; }
  th:nth-child(7) { width:80px; }
  th:nth-child(8) { width:140px; }
  th:nth-child(9) { width:130px; }
  th:nth-child(10){ width:190px; }
  td.name a { color:#1f2937; text-decoration:none; }
  td.name a:hover { color:#2563eb; text-decoration:underline; }
  td.name, td.cmd, td.result { white-space:nowrap; overflow:hidden; text-overflow:ellipsis; }
  td.cmd { font-family:monospace; }
  td.actions { white-space:nowrap; }
  .badge { padding:2px 8px; border-radius:10px; font-size:11px; font-weight:600; }
  .pending { background:#fef3c7; color:#92400e; }
  .running { background:#dbeafe; color:#1e40af; }
  .success { background:#d1fae5; color:#065f46; }
  .failed, .dead { background:#fee2e2; color:#991b1b; }
  .stopped { background:#fef3c7; color:#92400e; }
  .paused { background:#e5e7eb; color:#374151; }
  .empty { padding:40px; text-align:center; color:#9ca3af; }
  button { padding:3px 8px; font-size:11px; border:1px solid #d1d5db; background:#fff; border-radius:4px; cursor:pointer; }
  button:hover { background:#f3f4f6; }
  button:disabled { opacity:.5; cursor:not-allowed; }
  button.danger { color:#991b1b; border-color:#fecaca; }
  button.danger:hover { background:#fee2e2; }
  .tabs { display:flex; gap:6px; margin-bottom:12px; flex-wrap:wrap; }
  .tabs a { padding:5px 12px; border:1px solid #d1d5db; border-radius:16px; font-size:12px; text-decoration:none; color:#374151; background:#fff; }
  .tabs a:hover { background:#f3f4f6; }
  .tabs a.active { background:#2563eb; color:#fff; border-color:#2563eb; }
  .search { display:grid; grid-template-columns:repeat(auto-fit,minmax(140px,1fr)); gap:8px; margin-bottom:12px; padding:12px; background:#f9fafb; border:1px solid #e5e7eb; border-radius:8px; }
  .search label { font-size:11px; color:#6b7280; display:block; margin-bottom:2px; }
  .search input, .search select { width:100%; padding:5px 8px; border:1px solid #d1d5db; border-radius:4px; font-size:12px; box-sizing:border-box; }
  .search .actions { display:flex; align-items:flex-end; gap:6px; }
  .search button { padding:5px 12px; font-size:12px; }
  #toast { position:fixed; bottom:24px; right:24px; padding:10px 16px; border-radius:6px; color:#fff; font-size:13px; opacity:0; transition:opacity .3s; z-index:999; }
  #toast.ok { background:#059669; }
  #toast.err { background:#dc2626; }
  #toast.show { opacity:1; }
  .pagination { display:flex; align-items:center; gap:6px; margin-top:16px; flex-wrap:wrap; }
  .pagination button { padding:5px 12px; font-size:12px; border:1px solid #d1d5db; background:#fff; border-radius:4px; cursor:pointer; color:#374151; }
  .pagination button:hover:not(:disabled) { background:#f3f4f6; }
  .pagination button.active { background:#2563eb; color:#fff; border-color:#2563eb; }
  .pagination button:disabled { opacity:.4; cursor:not-allowed; }
  .pagination .ellipsis { padding:0 4px; color:#9ca3af; font-size:13px; }
  .pagination .page-info { font-size:12px; color:#6b7280; margin:0 8px; }
  .pagination .page-size { padding:4px 6px; font-size:12px; border:1px solid #d1d5db; border-radius:4px; }
  .modal-overlay { display:none; position:fixed; inset:0; background:rgba(0,0,0,.45); z-index:1000; justify-content:center; align-items:center; }
  .modal-overlay.show { display:flex; }
  .modal { background:#fff; border-radius:10px; width:480px; max-width:92vw; max-height:90vh; overflow-y:auto; box-shadow:0 10px 40px rgba(0,0,0,.2); }
  .modal-header { display:flex; justify-content:space-between; align-items:center; padding:16px 20px; border-bottom:1px solid #e5e7eb; }
  .modal-header h3 { margin:0; font-size:16px; color:#111827; }
  .modal-close { background:none; border:none; font-size:22px; color:#9ca3af; cursor:pointer; line-height:1; }
  .modal-close:hover { color:#374151; }
  .form-row { padding:12px 20px; }
  .form-row label { display:block; font-size:12px; color:#374151; font-weight:500; margin-bottom:5px; }
  .form-row .req { color:#dc2626; }
  .form-row input[type=text], .form-row input[type=number], .form-row textarea {
    width:100%; padding:7px 10px; border:1px solid #d1d5db; border-radius:5px; font-size:13px; box-sizing:border-box; font-family:inherit;
  }
  .form-row textarea { resize:vertical; font-family:monospace; }
  .form-row input:focus, .form-row textarea:focus { outline:none; border-color:#2563eb; box-shadow:0 0 0 2px rgba(37,99,235,.15); }
  .form-row .hint { display:block; font-size:11px; color:#9ca3af; margin-top:3px; }
  .form-row .field-err { display:block; font-size:11px; color:#dc2626; margin-top:4px; min-height:14px; }
  .form-row input.invalid, .form-row textarea.invalid { border-color:#dc2626; background:#fef2f2; }
  .modal-footer { display:flex; justify-content:flex-end; gap:8px; padding:14px 20px; border-top:1px solid #e5e7eb; }
</style>
</head>
<body>
<header>
  <h1>Task Scheduler <span>{{.Server}} · {{.Now}}</span></h1>
</header>
<main>
  {{if .ParentID}}
  <div style="margin-bottom:12px;padding:10px 14px;background:#eff6ff;border:1px solid #bfdbfe;border-radius:6px;font-size:13px;color:#1e40af">
    <a href="/" style="color:#2563eb;text-decoration:none">← 返回全部任务</a>
    <span style="margin:0 8px;color:#93c5fd">/</span>
    <b>{{.ParentName}} (ID: {{.ParentID}}) 的执行记录</b>
  </div>
  {{end}}
  <div style="margin-bottom:16px;display:flex;align-items:center;gap:12px;flex-wrap:wrap">
    <span class="stat"><b id="taskCount">{{.Total}}</b>total (page {{.Page}}/{{.TotalPages}})</span>
    <span class="stat"><b>{{.Server}}</b>endpoint</span>
    <button id="refreshBtn" onclick="refreshTable()" style="padding:5px 14px;font-size:12px;background:#2563eb;color:#fff;border:none;border-radius:4px;cursor:pointer">刷新</button>
    <button onclick="openCreateModal()" style="padding:5px 14px;font-size:12px;background:#059669;color:#fff;border:none;border-radius:4px;cursor:pointer">+ 新建任务</button>
    <label style="font-size:12px;color:#6b7280;display:flex;align-items:center;gap:4px">
      <input type="checkbox" id="autoRefresh" onchange="toggleAutoRefresh()"> 自动刷新
    </label>
    <span id="refreshTime" style="font-size:11px;color:#9ca3af"></span>
  </div>

  {{if .Workers}}
  <div style="margin-bottom:16px;background:#fff;border:1px solid #e5e7eb;border-radius:8px;overflow:hidden;box-shadow:0 1px 2px rgba(0,0,0,.04)">
    <div style="padding:10px 14px;background:#f9fafb;border-bottom:1px solid #e5e7eb;font-size:13px;font-weight:600;color:#374151;display:flex;justify-content:space-between;align-items:center">
      <span>工作节点 Workers ({{len .Workers}})</span>
      <span id="workerCount" style="font-size:11px;color:#6b7280;font-weight:normal"></span>
    </div>
    <table id="workerTable" style="box-shadow:none;border-radius:0">
      <thead>
        <tr><th style="width:120px">ID</th><th style="width:80px">槽位</th><th style="width:80px">活跃</th><th style="width:200px">运行中任务</th><th style="width:120px">运行时长</th><th style="width:150px">最后心跳</th></tr>
      </thead>
      <tbody id="workerBody">
      {{range .Workers}}
        <tr>
          <td>{{.ID}}</td>
          <td>{{.TotalSlots}}</td>
          <td>{{.ActiveCount}}/{{.TotalSlots}}</td>
          <td class="cmd" title="{{range $i,$v := .RunningTasks}}{{if $i}}, {{end}}{{$v}}{{end}}">{{if .RunningTasks}}{{range $i,$v := .RunningTasks}}{{if $i}}, {{end}}{{$v}}{{end}}{{else}}—{{end}}</td>
          <td>{{.StartedAt.Format "2006-01-02 15:04:05"}}</td>
          <td>{{.LastHeartbeat.Format "15:04:05"}}</td>
        </tr>
      {{end}}
      </tbody>
    </table>
  </div>
  {{else}}
  <div style="margin-bottom:16px;background:#fff;border:1px dashed #d1d5db;border-radius:8px;padding:14px;font-size:13px;color:#9ca3af;text-align:center">暂无在线工作节点</div>
  {{end}}

  <form class="search" method="GET" action="/" id="searchForm">
    {{if .ParentID}}<input type="hidden" name="parent_id" value="{{.ParentID}}">{{end}}
    <div><label>名称 Name</label><input type="text" name="name" value="{{if .Filter}}{{.Filter.Name}}{{end}}" placeholder="模糊匹配"></div>
    <div><label>命令 Command</label><input type="text" name="command" value="{{if .Filter}}{{.Filter.Command}}{{end}}" placeholder="模糊匹配"></div>
    <div><label>类型 Type</label>
      <select name="type">
        <option value="">全部</option>
        <option value="once"{{if and .Filter (eq .Filter.Type "once")}} selected{{end}}>一次性 once</option>
        <option value="cron"{{if and .Filter (eq .Filter.Type "cron")}} selected{{end}}>定时 cron</option>
      </select>
    </div>
    <div><label>Worker</label><input type="text" name="worker" value="{{if .Filter}}{{.Filter.Worker}}{{end}}" placeholder="worker id 模糊匹配"></div>
    <div><label>启用 Enabled</label>
      <select name="enabled">
        <option value="">全部</option>
        <option value="true"{{if and .Filter (eq .Filter.Enabled "true")}} selected{{end}}>启用</option>
        <option value="false"{{if and .Filter (eq .Filter.Enabled "false")}} selected{{end}}>停用</option>
      </select>
    </div>
    <div class="actions">
      <button type="submit">搜索</button>
      <button type="button" onclick="resetSearch()">重置</button>
    </div>
  </form>

  <div class="tabs">
    <a href="#" data-status="all"     {{if eq .CurrentStatus "all"}}class="active"{{end}}>全部</a>
    <a href="#" data-status="pending" {{if eq .CurrentStatus "pending"}}class="active"{{end}}>待执行</a>
    <a href="#" data-status="running" {{if eq .CurrentStatus "running"}}class="active"{{end}}>执行中</a>
    <a href="#" data-status="success" {{if eq .CurrentStatus "success"}}class="active"{{end}}>已完成</a>
    <a href="#" data-status="failed"  {{if eq .CurrentStatus "failed"}}class="active"{{end}}>失败</a>
    <a href="#" data-status="stopped" {{if eq .CurrentStatus "stopped"}}class="active"{{end}}>已停止</a>
    <a href="#" data-status="done"    {{if eq .CurrentStatus "done"}}class="active"{{end}}>已结束</a>
  </div>
  {{if .Tasks}}
  <table>
    <thead>
      <tr><th>ID</th><th>Name</th><th>Command</th><th>Type</th><th>Status</th><th>Enabled</th><th>Worker</th><th>Result</th><th>Updated</th><th>Actions</th></tr>
    </thead>
    <tbody id="taskBody">
    {{range .Tasks}}
      <tr>
        <td>{{.ID}}</td>
        <td class="name" title="{{.Name}}"><a href="/tasks/{{.ID}}">{{.Name}}</a></td>
        <td class="cmd" title="{{.Command}}">{{.Command}}</td>
        <td>{{.Type}}</td>
        <td><span class="badge {{.Status}}">{{.Status}}</span></td>
        <td>{{if .Enabled}}yes{{else}}<span class="badge paused">paused</span>{{end}}</td>
        <td>{{.Worker}}</td>
        <td class="result" title="{{.Result}}">{{.Result}}</td>
        <td>{{.Updated}}</td>
        <td class="actions">
          {{if eq .Type "cron"}}
            <a href="/?parent_id={{.ID}}" style="margin-right:6px;font-size:11px;color:#2563eb;text-decoration:none">执行记录</a>
            {{if .Enabled}}<button onclick="taskAction({{.ID}},'pause',this)">Pause</button>
            {{else}}<button onclick="taskAction({{.ID}},'resume',this)">Resume</button>{{end}}
          {{else}}
            {{if eq .Status "stopped"}}
              <button onclick="taskAction({{.ID}},'restart',this)">Restart</button>
            {{else if or (eq .Status "running") (eq .Status "pending")}}
              <button onclick="taskAction({{.ID}},'stop',this)" class="danger">Stop</button>
            {{end}}
          {{end}}
          <button onclick="taskAction({{.ID}},'delete',this)" class="danger">Delete</button>
        </td>
      </tr>
    {{end}}
    </tbody>
  </table>
  {{else}}
  <div class="empty">No tasks yet. POST /api/tasks to create one.</div>
  {{end}}

  {{if gt .TotalPages 1}}
  <div id="pagination" class="pagination">
    <button onclick="goPage({{.Page}}-1)" {{if le .Page 1}}disabled{{end}}>« 上一页</button>
    {{range .Pages}}
      {{if .Ellipsis}}
        <span class="ellipsis">…</span>
      {{else}}
        <button class="{{if .Current}}active{{end}}" onclick="goPage({{.Page}})">{{.Page}}</button>
      {{end}}
    {{end}}
    <button onclick="goPage({{.Page}}+1)" {{if ge .Page .TotalPages}}disabled{{end}}>下一页 »</button>
    <span class="page-info">共 {{.Total}} 条 / {{.TotalPages}} 页</span>
    <select onchange="changePageSize(this.value)" class="page-size">
      <option value="10" {{if eq .PageSize 10}}selected{{end}}>10/页</option>
      <option value="20" {{if eq .PageSize 20}}selected{{end}}>20/页</option>
      <option value="50" {{if eq .PageSize 50}}selected{{end}}>50/页</option>
      <option value="100" {{if eq .PageSize 100}}selected{{end}}>100/页</option>
    </select>
  </div>
  {{end}}
</main>
<div id="toast"></div>

<!-- Create task modal -->
<div id="createModal" class="modal-overlay" onclick="if(event.target===this)closeCreateModal()">
  <div class="modal">
    <div class="modal-header">
      <h3>新建任务</h3>
      <button class="modal-close" onclick="closeCreateModal()">&times;</button>
    </div>
    <form id="createForm" onsubmit="return submitCreateTask(event)" novalidate>
      <div class="form-row">
        <label>名称 Name <span class="req">*</span></label>
        <input type="text" name="name" placeholder="任务名称" maxlength="200" oninput="clearFieldError(this)">
        <span class="field-err" data-for="name"></span>
      </div>
      <div class="form-row">
        <label>命令 Command <span class="req">*</span></label>
        <textarea name="command" rows="3" placeholder="shell 命令，如 echo hello" oninput="clearFieldError(this)"></textarea>
        <span class="field-err" data-for="command"></span>
      </div>
      <div class="form-row">
        <label>类型 Type <span class="req">*</span></label>
        <div style="display:flex;gap:16px" onchange="clearFieldError('type');toggleCronField()">
          <label style="font-weight:normal;display:flex;align-items:center;gap:4px"><input type="radio" name="type" value="once" checked> 一次性 once</label>
          <label style="font-weight:normal;display:flex;align-items:center;gap:4px"><input type="radio" name="type" value="cron"> 定时 cron</label>
        </div>
        <span class="field-err" data-for="type"></span>
      </div>
      <div class="form-row" id="cronRow" style="display:none">
        <label>Cron 表达式 <span class="req">*</span></label>
        <input type="text" name="cron_expr" placeholder="6 字段，含秒，如 */10 * * * * *" oninput="clearFieldError(this)">
        <span class="hint">格式：秒 分 时 日 月 周</span>
        <span class="field-err" data-for="cron_expr"></span>
      </div>
      <div class="form-row">
        <label>最大重试次数 MaxRetry</label>
        <input type="number" name="max_retry" value="0" min="0" max="100" oninput="clearFieldError(this)">
        <span class="field-err" data-for="max_retry"></span>
      </div>
      <div class="form-row">
        <label>超时秒数 TimeoutSec</label>
        <input type="number" name="timeout_sec" value="300" min="0" placeholder="0 表示无超时" oninput="clearFieldError(this)">
        <span class="field-err" data-for="timeout_sec"></span>
      </div>
      <div class="modal-footer">
        <button type="button" onclick="closeCreateModal()" style="padding:6px 16px;font-size:13px;background:#e5e7eb;border:none;border-radius:4px;cursor:pointer">取消</button>
        <button type="submit" id="createSubmitBtn" style="padding:6px 16px;font-size:13px;background:#059669;color:#fff;border:none;border-radius:4px;cursor:pointer">创建</button>
      </div>
    </form>
  </div>
</div>

<script>
  // ---- Local (no full-page-reload) refresh ----
  // refreshTable fetches the task list via the API and re-renders only the
  // table body, preserving the current filter (search form + status tab).
  var autoTimer = null;
  var currentPage = {{.Page}};
  var currentPageSize = {{.PageSize}};
  // AUTH_TOKEN is injected by the server; empty when auth is disabled.
  // Every API fetch must include it as the Authorization bearer header.
  var AUTH_TOKEN = {{if .Token}}"{{.Token}}"{{else}}""{{end}};
  function authHeaders(){
    var h = {};
    if (AUTH_TOKEN) h['Authorization'] = 'Bearer ' + AUTH_TOKEN;
    return h;
  }

  function escapeHtml(s){
    if (s == null) return '';
    return String(s).replace(/[&<>"']/g, function(c){
      return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c];
    });
  }

  function renderRow(t){
    var enabledCell = t.enabled ? 'yes' : '<span class="badge paused">paused</span>';
    var actions = '';
    if (t.type === 'cron') {
      actions += '<a href="/?parent_id='+t.id+'" style="margin-right:6px;font-size:11px;color:#2563eb;text-decoration:none">执行记录</a>';
      if (t.enabled) actions += '<button onclick="taskAction('+t.id+',\'pause\',this)">Pause</button>';
      else actions += '<button onclick="taskAction('+t.id+',\'resume\',this)">Resume</button>';
    } else {
      if (t.status === 'stopped') {
        actions += '<button onclick="taskAction('+t.id+',\'restart\',this)">Restart</button>';
      } else if (t.status === 'running' || t.status === 'pending') {
        actions += '<button onclick="taskAction('+t.id+',\'stop\',this)" class="danger">Stop</button>';
      }
    }
    actions += '<button onclick="taskAction('+t.id+',\'delete\',this)" class="danger">Delete</button>';
    return '<tr>'
      + '<td>'+t.id+'</td>'
      + '<td class="name" title="'+escapeHtml(t.name)+'"><a href="/tasks/'+t.id+'">'+escapeHtml(t.name)+'</a></td>'
      + '<td class="cmd" title="'+escapeHtml(t.command)+'">'+escapeHtml(t.command)+'</td>'
      + '<td>'+t.type+'</td>'
      + '<td><span class="badge '+t.status+'">'+t.status+'</span></td>'
      + '<td>'+enabledCell+'</td>'
      + '<td>'+escapeHtml(t.worker_id||'')+'</td>'
      + '<td class="result" title="'+escapeHtml(t.result||'')+'">'+escapeHtml(t.result||'')+'</td>'
      + '<td>'+escapeHtml(t.updated_at||'')+'</td>'
      + '<td class="actions">'+actions+'</td>'
      + '</tr>';
  }

  function renderRows(tasks){
    var tbody = document.getElementById('taskBody');
    if (!tbody) return;
    if (!tasks || tasks.length === 0) {
      tbody.innerHTML = '<tr><td colspan="10" class="empty" style="text-align:center;padding:40px;color:#9ca3af">No tasks match the current filter.</td></tr>';
      return;
    }
    tbody.innerHTML = tasks.map(renderRow).join('');
  }

  // renderPagination rebuilds the pagination bar from the API response.
  // Mirrors the server-side buildPageItems logic for the JS-driven refresh.
  function renderPagination(data){
    var el = document.getElementById('pagination');
    document.getElementById('taskCount').textContent = data.total;
    if (data.total_pages <= 1) {
      if (el) el.style.display = 'none';
      return;
    }
    if (el) el.style.display = 'flex';
    currentPage = data.page;
    currentPageSize = data.page_size;
    var total = data.total_pages, cur = data.page;
    var lo = Math.max(1, cur - 2), hi = Math.min(total, cur + 2);
    var html = '<button onclick="goPage('+(cur-1)+')" '+(cur<=1?'disabled':'')+'>« 上一页</button>';
    if (lo > 1) {
      html += '<button onclick="goPage(1)">1</button>';
      if (lo > 2) html += '<span class="ellipsis">…</span>';
    }
    for (var p = lo; p <= hi; p++) {
      html += '<button class="'+(p===cur?'active':'')+'" onclick="goPage('+p+')">'+p+'</button>';
    }
    if (hi < total) {
      if (hi < total-1) html += '<span class="ellipsis">…</span>';
      html += '<button onclick="goPage('+total+')">'+total+'</button>';
    }
    html += '<button onclick="goPage('+(cur+1)+')" '+(cur>=total?'disabled':'')+'>下一页 »</button>';
    html += '<span class="page-info">共 '+data.total+' 条 / '+total+' 页</span>';
    html += '<select onchange="changePageSize(this.value)" class="page-size">';
    [10,20,50,100].forEach(function(sz){
      html += '<option value="'+sz+'" '+(sz===data.page_size?'selected':'')+'>'+sz+'/页</option>';
    });
    html += '</select>';
    el.innerHTML = html;
  }

  function currentQueryString(){
    var fd = new FormData(document.getElementById('searchForm'));
    var params = new URLSearchParams();
    fd.forEach(function(v, k){ if (v) params.set(k, v); });
    var status = document.querySelector('.tabs a.active');
    if (status) {
      var s = status.dataset.status;
      if (s && s !== 'all') params.set('status', s);
    }
    if (currentPage > 1) params.set('page', currentPage);
    if (currentPageSize !== 20) params.set('page_size', currentPageSize);
    return params.toString();
  }

  function goPage(n){
    if (n < 1) n = 1;
    currentPage = n;
    refreshTable();
  }

  function changePageSize(sz){
    currentPageSize = parseInt(sz, 10) || 20;
    currentPage = 1; // reset to first page when changing page size
    refreshTable();
  }

  function refreshTable(){
    var btn = document.getElementById('refreshBtn');
    btn.disabled = true; btn.textContent = '刷新中...';
    var qs = currentQueryString();
    var url = '/api/tasks' + (qs ? '?' + qs : '');
    fetch(url, {headers: authHeaders()}).then(function(res){ return res.json(); })
      .then(function(data){
        renderRows(data.tasks || []);
        renderPagination(data);
        document.getElementById('refreshTime').textContent = '上次刷新: ' + new Date().toLocaleTimeString();
      })
      .catch(function(err){ showToast('刷新失败: '+err.message,'err'); })
      .finally(function(){ btn.disabled = false; btn.textContent = '刷新'; });
    refreshWorkers();
  }

  function refreshWorkers(){
    fetch('/api/workers', {headers: authHeaders()}).then(function(res){ return res.json(); })
      .then(function(data){
        var tbody = document.getElementById('workerBody');
        if (!tbody) return;
        var rows = '';
        (data || []).forEach(function(w){
          var tasks = (w.running_tasks || []).join(', ');
          var started = w.started_at ? new Date(w.started_at).toLocaleString() : '';
          var hb = w.last_heartbeat ? new Date(w.last_heartbeat).toLocaleTimeString() : '';
          rows += '<tr>'
            + '<td>'+escapeHtml(w.id)+'</td>'
            + '<td>'+w.total_slots+'</td>'
            + '<td>'+w.active_count+'/'+w.total_slots+'</td>'
            + '<td class="cmd" title="'+escapeHtml(tasks)+'">'+(tasks || '—')+'</td>'
            + '<td>'+started+'</td>'
            + '<td>'+hb+'</td>'
            + '</tr>';
        });
        tbody.innerHTML = rows;
        var cnt = document.getElementById('workerCount');
        if (cnt) cnt.textContent = '共 ' + (data || []).length + ' 个';
      })
      .catch(function(){ /* worker list is best-effort */ });
  }

  function toggleAutoRefresh(){
    var on = document.getElementById('autoRefresh').checked;
    if (autoTimer) { clearInterval(autoTimer); autoTimer = null; }
    if (on) {
      refreshTable();
      autoTimer = setInterval(refreshTable, 3000);
      showToast('已开启自动刷新（3秒）','ok');
    } else {
      showToast('已关闭自动刷新','ok');
    }
  }

  // Search form: submit without reloading the whole page.
  document.getElementById('searchForm').addEventListener('submit', function(e){
    e.preventDefault();
    var qs = currentQueryString();
    history.replaceState(null, '', qs ? '/?' + qs : '/');
    refreshTable();
  });

  // Status tabs: switch the active filter and re-render locally.
  document.querySelectorAll('.tabs a').forEach(function(a){
    a.addEventListener('click', function(e){
      e.preventDefault();
      document.querySelectorAll('.tabs a').forEach(function(x){ x.classList.remove('active'); });
      a.classList.add('active');
      var qs = currentQueryString();
      history.replaceState(null, '', qs ? '/?' + qs : '/');
      refreshTable();
    });
  });

  function resetSearch(){
    var form = document.getElementById('searchForm');
    // Reset text/search inputs but keep the parent_id hidden field so we
    // stay on the same cron's execution-history view instead of jumping
    // back to the full top-level list.
    form.querySelectorAll('input:not([type="hidden"])').forEach(function(i){ i.value = ''; });
    form.querySelectorAll('select').forEach(function(s){ s.selectedIndex = 0; });
    document.querySelectorAll('.tabs a').forEach(function(x){ x.classList.remove('active'); });
    var allTab = document.querySelector('.tabs a[data-status="all"]');
    if (allTab) allTab.classList.add('active');
    currentPage = 1;
    var qs = currentQueryString();
    history.replaceState(null, '', qs ? '/?' + qs : '/');
    refreshTable();
  }

  // taskAction fires pause/resume/delete via fetch so the page never navigates
  // away to the raw JSON response. On success it reloads the list; on error
  // it shows a toast and re-enables the button.
  function taskAction(id, action, btn) {
    var path;
    var method;
    if (action === 'delete') {
      if (!confirm('Delete task ' + id + '?')) return;
      path   = '/api/tasks/' + id;
      method = 'DELETE';
    } else {
      path   = '/api/tasks/' + id + '/' + action;  // pause | resume
      method = 'POST';
    }
    btn.disabled = true;
    fetch(path, { method: method, headers: authHeaders() })
      .then(function(res) {
        if (!res.ok) throw new Error('HTTP ' + res.status);
        showToast(action + ' OK', 'ok');
        refreshTable();
      })
      .catch(function(err) {
        showToast(action + ' failed: ' + err.message, 'err');
        btn.disabled = false;
      });
  }

  var toastTimer;
  function showToast(msg, kind) {
    var el = document.getElementById('toast');
    el.textContent = msg;
    el.className = kind === 'ok' ? 'show ok' : 'show err';
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function(){ el.className = ''; }, 2500);
  }

  // ---- Create task modal ----
  function openCreateModal(){
    document.getElementById('createModal').classList.add('show');
    var form = document.getElementById('createForm');
    form.reset();
    // reset defaults
    document.querySelector('input[name="type"][value="once"]').checked = true;
    document.querySelector('input[name="max_retry"]').value = 0;
    document.querySelector('input[name="timeout_sec"]').value = 300;
    clearAllFieldErrors();
    toggleCronField();
    document.querySelector('#createForm input[name="name"]').focus();
  }

  function closeCreateModal(){
    document.getElementById('createModal').classList.remove('show');
  }

  function toggleCronField(){
    var isCron = document.querySelector('input[name="type"]:checked').value === 'cron';
    var row = document.getElementById('cronRow');
    row.style.display = isCron ? 'block' : 'none';
    // switching type hides any existing cron error
    if (!isCron) clearFieldError('cron_expr');
  }

  // clearFieldError clears the inline error for a field. Accepts either the
  // input element (from oninput) or the field name string.
  function clearFieldError(field){
    var name = typeof field === 'string' ? field : field.name;
    var el = document.querySelector('.field-err[data-for="'+name+'"]');
    if (el) el.textContent = '';
    var input = document.querySelector('#createForm [name="'+name+'"]');
    if (input) input.classList.remove('invalid');
  }

  function clearAllFieldErrors(){
    document.querySelectorAll('.field-err').forEach(function(e){ e.textContent = ''; });
    document.querySelectorAll('#createForm input, #createForm textarea').forEach(function(i){ i.classList.remove('invalid'); });
  }

  function setFieldError(name, msg){
    var el = document.querySelector('.field-err[data-for="'+name+'"]');
    if (el) el.textContent = msg;
    var input = document.querySelector('#createForm [name="'+name+'"]');
    if (input) input.classList.add('invalid');
  }

  // isValidCronExpr checks that the expression has exactly 6 whitespace-
  // separated fields. It does not validate each field's range (the server
  // does that), but catches the common mistake of using 5-field cron.
  function isValidCronExpr(expr){
    var fields = expr.trim().split(/\s+/);
    if (fields.length !== 6) return false;
    // each field must be non-empty and contain only cron-allowed chars
    var re = /^[0-9\*\/\-,?]+$/;
    return fields.every(function(f){ return re.test(f); });
  }

  function validateCreateForm(fd){
    clearAllFieldErrors();
    var ok = true;

    var name = (fd.get('name') || '').trim();
    if (!name) { setFieldError('name', '请输入任务名称'); ok = false; }
    else if (name.length > 200) { setFieldError('name', '名称不能超过 200 个字符'); ok = false; }

    var command = (fd.get('command') || '').trim();
    if (!command) { setFieldError('command', '请输入命令'); ok = false; }

    var type = fd.get('type');
    if (type !== 'once' && type !== 'cron') { setFieldError('type', '请选择任务类型'); ok = false; }

    if (type === 'cron') {
      var cron = (fd.get('cron_expr') || '').trim();
      if (!cron) { setFieldError('cron_expr', '请输入 Cron 表达式'); ok = false; }
      else if (!isValidCronExpr(cron)) { setFieldError('cron_expr', 'Cron 表达式格式错误，应为 6 个字段（秒 分 时 日 月 周）'); ok = false; }
    }

    var maxRetry = fd.get('max_retry');
    if (maxRetry === '' || maxRetry === null) { setFieldError('max_retry', '请输入最大重试次数'); ok = false; }
    else {
      var mr = parseInt(maxRetry, 10);
      if (isNaN(mr) || mr < 0 || mr > 100) { setFieldError('max_retry', '重试次数需为 0-100 的整数'); ok = false; }
    }

    var timeout = fd.get('timeout_sec');
    if (timeout === '' || timeout === null) { setFieldError('timeout_sec', '请输入超时秒数'); ok = false; }
    else {
      var ts = parseInt(timeout, 10);
      if (isNaN(ts) || ts < 0) { setFieldError('timeout_sec', '超时秒数需为 >= 0 的整数'); ok = false; }
    }

    return ok;
  }

  function submitCreateTask(e){
    e.preventDefault();
    var form = document.getElementById('createForm');
    var fd = new FormData(form);
    if (!validateCreateForm(fd)) {
      showToast('请修正表单中的错误', 'err');
      return false;
    }
    var type = fd.get('type');
    var payload = {
      name: fd.get('name').trim(),
      command: fd.get('command').trim(),
      type: type,
      cron_expr: type === 'cron' ? fd.get('cron_expr').trim() : '',
      max_retry: parseInt(fd.get('max_retry'), 10),
      timeout_sec: parseInt(fd.get('timeout_sec'), 10)
    };
    var btn = document.getElementById('createSubmitBtn');
    btn.disabled = true; btn.textContent = '创建中...';
    var createHeaders = {'Content-Type': 'application/json'};
    if (AUTH_TOKEN) createHeaders['Authorization'] = 'Bearer ' + AUTH_TOKEN;
    fetch('/api/tasks', {
      method: 'POST',
      headers: createHeaders,
      body: JSON.stringify(payload)
    }).then(function(res){
      if (!res.ok) {
        return res.text().then(function(t){ throw new Error(t || res.statusText); });
      }
      return res.json();
    }).then(function(){
      showToast('任务创建成功', 'ok');
      closeCreateModal();
      // new tasks land on page 1, so reset pagination before refreshing
      currentPage = 1;
      refreshTable();
    }).catch(function(err){
      showToast('创建失败: ' + err.message, 'err');
    }).finally(function(){
      btn.disabled = false; btn.textContent = '创建';
    });
    return false;
  }
</script>
</body>
</html>`))

// detailTmpl renders the single-task detail page. Shares the same visual
// language as uiTmpl so navigation between list and detail feels seamless.
var detailTmpl = template.Must(template.New("detail").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Task #{{.Task.ID}} · Task Scheduler</title>
<style>
  body { font-family: -apple-system, Segoe UI, Roboto, sans-serif; margin: 0; background:#f6f7f9; color:#222; }
  header { background:#1f2937; color:#fff; padding:16px 24px; display:flex; justify-content:space-between; align-items:center; }
  header h1 { margin:0; font-size:20px; font-weight:600; }
  header h1 span { opacity:.7; font-size:13px; margin-left:8px; }
  main { max-width:1100px; margin:24px auto; padding:0 16px; }
  .crumb { margin-bottom:16px; font-size:13px; }
  .crumb a { color:#2563eb; text-decoration:none; }
  .crumb a:hover { text-decoration:underline; }
  .card { background:#fff; border:1px solid #e5e7eb; border-radius:8px; padding:20px 24px; box-shadow:0 1px 2px rgba(0,0,0,.04); margin-bottom:20px; }
  .card h2 { margin:0 0 16px; font-size:16px; color:#111827; }
  dl.grid { display:grid; grid-template-columns:140px 1fr; gap:10px 16px; margin:0; }
  dl.grid dt { color:#6b7280; font-size:13px; font-weight:500; padding-top:2px; }
  dl.grid dd { margin:0; font-size:13px; word-break:break-all; }
  dl.grid dd code, dl.grid dd pre { background:#f3f4f6; padding:2px 6px; border-radius:4px; font-size:12px; }
  dl.grid dd pre { white-space:pre-wrap; margin:0; padding:8px 10px; }
  .badge { padding:2px 8px; border-radius:10px; font-size:11px; font-weight:600; }
  .pending { background:#fef3c7; color:#92400e; }
  .running { background:#dbeafe; color:#1e40af; }
  .success { background:#d1fae5; color:#065f46; }
  .failed, .dead { background:#fee2e2; color:#991b1b; }
  .stopped { background:#fef3c7; color:#92400e; }
  table { width:100%; border-collapse:collapse; background:#fff; border-radius:8px; overflow:hidden; box-shadow:0 1px 2px rgba(0,0,0,.04); table-layout:fixed; }
  th, td { padding:10px 12px; text-align:left; border-bottom:1px solid #f1f1f1; font-size:13px; }
  th { background:#f9fafb; font-weight:600; color:#374151; }
  td.cmd { font-family:monospace; white-space:nowrap; overflow:hidden; text-overflow:ellipsis; }
  th:nth-child(1){width:60px} th:nth-child(2){width:160px} th:nth-child(3){width:220px}
  th:nth-child(4){width:80px} th:nth-child(5){width:80px} th:nth-child(6){width:180px}
</style>
</head>
<body>
<header>
  <h1>Task Scheduler <span>{{.Server}} · {{.Now}}</span></h1>
</header>
<main>
  <div class="crumb"><a href="/">← 返回全部任务</a></div>

  <div class="card">
    <h2>任务详情 #{{.Task.ID}}</h2>
    <dl class="grid">
      <dt>ID</dt><dd>{{.Task.ID}}</dd>
      <dt>名称 Name</dt><dd>{{.Task.Name}}</dd>
      <dt>类型 Type</dt><dd>{{.Task.Type}}</dd>
      <dt>状态 Status</dt><dd><span class="badge {{.Task.Status}}">{{.Task.Status}}</span></dd>
      <dt>启用 Enabled</dt><dd>{{if .Task.Enabled}}yes{{else}}no{{end}}</dd>
      <dt>Cron 表达式</dt><dd>{{if .Task.CronExpr}}<code>{{.Task.CronExpr}}</code>{{else}}—{{end}}</dd>
      <dt>最大重试</dt><dd>{{.Task.MaxRetry}}</dd>
      <dt>已重试次数</dt><dd>{{.Task.RetryCount}}</dd>
      <dt>超时(秒)</dt><dd>{{.Task.TimeoutSec}}</dd>
      <dt>Worker</dt><dd>{{if .Task.WorkerID}}{{.Task.WorkerID}}{{else}}—{{end}}</dd>
      <dt>父任务 ID</dt><dd>{{if .Task.ParentID}}{{.Task.ParentID}}{{else}}—{{end}}</dd>
      <dt>下次执行</dt><dd>{{if .Task.NextRunAt.IsZero}}—{{else}}{{.Task.NextRunAt.Format "2006-01-02 15:04:05"}}{{end}}</dd>
      <dt>创建时间</dt><dd>{{.Task.CreatedAt.Format "2006-01-02 15:04:05"}}</dd>
      <dt>更新时间</dt><dd>{{.Task.UpdatedAt.Format "2006-01-02 15:04:05"}}</dd>
      <dt>命令 Command</dt><dd><pre>{{.Task.Command}}</pre></dd>
      <dt>结果 Result</dt><dd><pre>{{if .Task.Result}}{{.Task.Result}}{{else}}—{{end}}</pre></dd>
    </dl>
  </div>

  {{if .Rows}}
  <div class="card">
    <h2>执行记录 ({{len .Rows}})</h2>
    <table>
      <thead>
        <tr><th>ID</th><th>Name</th><th>Command</th><th>Status</th><th>Worker</th><th>Updated</th></tr>
      </thead>
      <tbody>
      {{range .Rows}}
        <tr>
          <td>{{.ID}}</td>
          <td>{{.Name}}</td>
          <td class="cmd" title="{{.Command}}">{{.Command}}</td>
          <td><span class="badge {{.Status}}">{{.Status}}</span></td>
          <td>{{.Worker}}</td>
          <td>{{.Updated}}</td>
        </tr>
      {{end}}
      </tbody>
    </table>
  </div>
  {{end}}
</main>
</body>
</html>`))
