// Package server (ui.go) renders the in-process web dashboard via
// html/template. Keeping it server-side means zero JS build step and a
// single binary deploy.
package server

import (
	"net/http"
	"time"
)

// uiData is the template's view model.
type uiData struct {
	Tasks  []*taskRow
	Now    string
	Server string
}

// taskRow mirrors what we want to display; derived from store.Task.
type taskRow struct {
	ID, Name, Command, Type, Status, Worker, Result, Created, Updated string
}

func (s *Server) renderUI(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.store.List(r.Context(), 100)
	if err != nil {
		http.Error(w, "failed to load tasks", http.StatusInternalServerError)
		return
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
			Created:  t.CreatedAt.Format(time.RFC3339),
			Updated: t.UpdatedAt.Format(time.RFC3339),
		})
	}
	data := uiData{Tasks: rows, Now: time.Now().Format(time.RFC3339), Server: s.httpSrv.Addr}
	if err := uiTmpl.Execute(w, data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
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
