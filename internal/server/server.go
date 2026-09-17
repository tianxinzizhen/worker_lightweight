// Package server wires the HTTP API, the SQLite store and the cron scheduler
// into a single serveable component. Routes are mounted in ServeMux so the
// web UI and JSON API share one listener.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/tianxinzizhen/worker_lightweight/internal/lock"
	"github.com/tianxinzizhen/worker_lightweight/internal/logger"
	"github.com/tianxinzizhen/worker_lightweight/internal/model"
	"github.com/tianxinzizhen/worker_lightweight/internal/store"
)

// Server owns the store, scheduler and HTTP listener. All fields are set in
// New and read-only afterwards, so Server is safe for concurrent use.
type Server struct {
	store   *store.Store
	sched   *Scheduler
	httpSrv *http.Server
}

// New builds a Server bound to addr. Call Start to run the listener.
func New(addr, dbPath string, syncEvery time.Duration) (*Server, error) {
	st, err := store.New(dbPath)
	if err != nil {
		return nil, err
	}
	lk := lock.NewMemory()
	sch := NewScheduler(st, lk, syncEvery)
	mux := http.NewServeMux()
	s := &Server{store: st, sched: sch}
	s.register(mux)
	s.httpSrv = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	return s, nil
}

// register attaches all routes to the mux. Grouping them here keeps routing
// discoverable in one place.
func (s *Server) register(mux *http.ServeMux) {
	mux.HandleFunc("/api/tasks", s.handleTasks)
	mux.HandleFunc("/api/tasks/", s.handleTaskByID)
	mux.HandleFunc("/api/pull", s.handlePull)
	mux.HandleFunc("/api/result", s.handleResult)
	mux.HandleFunc("/", s.handleUI)
}

// Start runs the scheduler and HTTP listener until ctx is cancelled.
// Shutdown blocks in-progress requests up to 5s then returns.
func (s *Server) Start(ctx context.Context) error {
	go s.sched.Start(ctx)
	logger.L.Info("server listening", "addr", s.httpSrv.Addr)
	err := s.httpSrv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully stops the HTTP server and closes the store. The
// scheduler stops via the same ctx that cancels Start.
func (s *Server) Shutdown(ctx context.Context) error {
	logger.L.Info("server shutting down")
	return s.httpSrv.Shutdown(ctx)
}

// Close releases store resources after Shutdown completes.
func (s *Server) Close() error { return s.store.Close() }

// ---- helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ---- handlers --------------------------------------------------------------

// handleTasks covers POST /api/tasks (create) and GET /api/tasks (list).
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.createTask(w, r)
	case http.MethodGet:
		s.listTasks(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req model.CreateTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if req.Name == "" || req.Command == "" {
		writeErr(w, http.StatusBadRequest, "name and command required")
		return
	}
	t := model.ScheduleType(req.Type)
	if t == "" {
		t = model.ScheduleOnce
	}
	if t != model.ScheduleOnce && t != model.ScheduleCron {
		writeErr(w, http.StatusBadRequest, "type must be once or cron")
		return
	}
	if t == model.ScheduleCron && req.CronExpr == "" {
		writeErr(w, http.StatusBadRequest, "cron tasks require cron_expr")
		return
	}
	task := &model.Task{
		Name:       req.Name,
		Command:    req.Command,
		Type:       t,
		CronExpr:   req.CronExpr,
		Status:     model.StatusPending,
		MaxRetry:   req.MaxRetry,
		TimeoutSec: req.TimeoutSec,
	}
	if err := s.store.Create(r.Context(), task); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, task)
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	tasks, err := s.store.List(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

// handleTaskByID routes GET /api/tasks/{id}.
func (s *Server) handleTaskByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	idStr := r.URL.Path[len("/api/tasks/"):]
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	t, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// handlePull is the worker's main entry: atomically claim the next due
// pending task. Returns 204 when there's nothing to run so the worker can
// back off cleanly.
func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	workerID := r.URL.Query().Get("worker_id")
	if workerID == "" {
		writeErr(w, http.StatusBadRequest, "worker_id required")
		return
	}
	t, err := s.store.ClaimPending(r.Context(), workerID)
	if errors.Is(err, store.ErrNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// handleResult accepts the worker's report after execution.
func (s *Server) handleResult(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var res model.Result
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := s.store.ReportResult(r.Context(), &res); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "task not found")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleUI renders the simple task dashboard. Implemented in ui.go.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	s.renderUI(w, r)
}
