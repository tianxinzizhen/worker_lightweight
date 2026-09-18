// Package server (ui.go) renders the in-process web dashboard via
// html/template. Keeping it server-side means zero JS build step and a
// single binary deploy.
package server

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tianxinzizhen/worker_lightweight/internal/logger"
	"github.com/tianxinzizhen/worker_lightweight/internal/model"
	"github.com/tianxinzizhen/worker_lightweight/internal/store"
)

// uiData is the template's view model.
type uiData struct {
	Tasks         []*taskRow
	Workers       []*model.WorkerInfo
	Now           string
	Server        string
	Token         string // embedded so JS fetch calls can authenticate
	CurrentStatus string // active status filter tab, e.g. "all", "pending"
	Filter        *store.ListFilter
	ParentID      int64  // 0 = all top-level tasks; >0 = viewing that cron's children
	ParentName    string // name of the cron def whose children we're viewing (for breadcrumb)
	Page          int
	PageSize      int
	Total         int64
	TotalPages    int
	Pages         []pageItem
}

// taskRow mirrors what we want to display; derived from store.Task.
type taskRow struct {
	ID, Name, Command, Type, Status, Worker, Result, Created, Updated string
	Enabled                                                           bool
}

// pageItem represents a single clickable page button or an ellipsis gap in
// the pagination bar. Pre-computed in Go so the template stays simple.
type pageItem struct {
	Page     int  // page number (1-based); ignored when Ellipsis is true
	Current  bool // true for the active page
	Ellipsis bool // true renders "…" instead of a button
}

// buildPageItems returns the page numbers to display, with ellipsis (-1) for
// gaps far from the current page. Always includes first and last pages.
func buildPageItems(current, total int) []pageItem {
	if total <= 1 {
		return nil
	}
	var items []pageItem
	add := func(p int) {
		if p == -1 {
			items = append(items, pageItem{Ellipsis: true})
			return
		}
		items = append(items, pageItem{Page: p, Current: p == current})
	}
	// window of pages around current: [current-2, current+2]
	lo := current - 2
	hi := current + 2
	if lo < 1 {
		lo = 1
	}
	if hi > total {
		hi = total
	}
	if lo > 1 {
		add(1)
		if lo > 2 {
			add(-1)
		}
	}
	for p := lo; p <= hi; p++ {
		add(p)
	}
	if hi < total {
		if hi < total-1 {
			add(-1)
		}
		add(total)
	}
	return items
}

func (s *Server) renderUI(w http.ResponseWriter, r *http.Request) {
	// Gate the dashboard when a token is configured. The query-param form
	// lets users open the page directly from a browser address bar.
	if !s.uiAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	statusFilter := q.Get("status")
	if statusFilter == "all" {
		statusFilter = ""
	}
	parentID := parseParentID(q.Get("parent_id"))
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(q.Get("page_size"))
	if pageSize <= 0 || pageSize > 500 {
		pageSize = 20
	}
	f := &store.ListFilter{
		Name:     q.Get("name"),
		Command:  q.Get("command"),
		Type:     q.Get("type"),
		Status:   statusFilter,
		Worker:   q.Get("worker"),
		Enabled:  q.Get("enabled"),
		ParentID: parentID,
	}
	offset := (page - 1) * pageSize
	tasks, err := s.store.List(r.Context(), f, offset, pageSize)
	if err != nil {
		logger.L.Error("list tasks failed", "err", err)
		http.Error(w, "failed to load tasks", http.StatusInternalServerError)
		return
	}
	total, err := s.store.Count(r.Context(), f)
	if err != nil {
		logger.L.Error("count tasks failed", "err", err)
		http.Error(w, "failed to count tasks", http.StatusInternalServerError)
		return
	}
	totalPages := int(total) / pageSize
	if int(total)%pageSize != 0 {
		totalPages++
	}
	if totalPages < 1 {
		totalPages = 1
	}
	rows := make([]*taskRow, 0, len(tasks))
	for _, t := range tasks {
		rows = append(rows, &taskRow{
			ID:      itoa(t.ID),
			Name:    t.Name,
			Command: t.Command,
			Type:    string(t.Type),
			Status:  string(t.Status),
			Worker:  t.WorkerID,
			Result:  truncate(t.Result, 200),
			Created: t.CreatedAt.Format(time.RFC3339),
			Updated: t.UpdatedAt.Format(time.RFC3339),
			Enabled: t.Enabled,
		})
	}
	// When viewing a cron's children, resolve the parent's name for the
	// breadcrumb. A missing/non-cron parent just leaves the name blank.
	var parentName string
	if parentID > 0 {
		if p, err := s.store.Get(r.Context(), parentID); err == nil {
			parentName = p.Name
		}
	}
	currentStatus := statusFilter
	if currentStatus == "" {
		currentStatus = "all"
	}
	data := uiData{
		Tasks:         rows,
		Workers:       s.workers.List(),
		Now:           time.Now().Format(time.RFC3339),
		Server:        s.httpSrv.Addr,
		Token:         s.token,
		CurrentStatus: currentStatus,
		Filter:        f,
		ParentID:      parentID,
		ParentName:    parentName,
		Page:          page,
		PageSize:      pageSize,
		Total:         total,
		TotalPages:    totalPages,
		Pages:         buildPageItems(page, totalPages),
	}
	// Render into a buffer first: if the template fails partway through we
	// would otherwise have already committed a 200 to the wire, making the
	// subsequent http.Error trigger a superfluous WriteHeader warning.
	var buf bytes.Buffer
	if err := uiTmpl.Execute(&buf, data); err != nil {
		logger.L.Error("render error", "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(buf.Bytes())
}

// renderTaskDetail renders the single-task detail page at /tasks/{id}. It
// shows every field of the task and, for cron definitions, the list of
// child executions spawned so far.
func (s *Server) renderTaskDetail(w http.ResponseWriter, r *http.Request) {
	if !s.uiAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/tasks/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid task id", http.StatusBadRequest)
		return
	}
	t, err := s.store.Get(r.Context(), id)
	if err != nil {
		logger.L.Error("get task failed", "err", err, "id", id)
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}
	data := struct {
		Task   *model.Task
		Server string
		Now    string
		Token  string
		Rows   []*taskRow // children, only populated for cron defs
	}{
		Task:   t,
		Server: s.httpSrv.Addr,
		Now:    time.Now().Format(time.RFC3339),
		Token:  s.token,
	}
	// For a cron definition, load its execution history so the detail page
	// doubles as the per-cron run log.
	if t.Type == model.ScheduleCron {
		kids, err := s.store.List(r.Context(), &store.ListFilter{ParentID: t.ID}, 0, 100)
		if err == nil {
			for _, k := range kids {
				data.Rows = append(data.Rows, &taskRow{
					ID:      itoa(k.ID),
					Name:    k.Name,
					Command: k.Command,
					Type:    string(k.Type),
					Status:  string(k.Status),
					Worker:  k.WorkerID,
					Result:  k.Result,
					Created: k.CreatedAt.Format(time.RFC3339),
					Updated: k.UpdatedAt.Format(time.RFC3339),
					Enabled: k.Enabled,
				})
			}
		}
	}
	var buf bytes.Buffer
	if err := detailTmpl.Execute(&buf, data); err != nil {
		logger.L.Error("detail render error", "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(buf.Bytes())
}

func itoa(i int64) string {
	// Avoid strconv import clash; tiny helper.
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
