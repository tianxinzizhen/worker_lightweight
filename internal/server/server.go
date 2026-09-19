// Package server wires the HTTP API, the SQLite store and the cron scheduler
// into a single serveable component. Routes are mounted in ServeMux so the
// web UI and JSON API share one listener.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/tianxinzizhen/worker_lightweight/internal/lock"
	"github.com/tianxinzizhen/worker_lightweight/internal/logger"
	"github.com/tianxinzizhen/worker_lightweight/internal/model"
	"github.com/tianxinzizhen/worker_lightweight/internal/store"
)

// workerStaleAfter is how long a worker can go without a heartbeat before the
// server considers it offline and hides it from the dashboard.
const workerStaleAfter = 60 * time.Second

// workerRegistry keeps the latest WorkerInfo snapshot reported by each live
// worker. It is in-memory only: workers are ephemeral and re-register on
// restart, so there is no value in persisting them.
type workerRegistry struct {
	mu      sync.RWMutex
	entries map[string]*model.WorkerInfo
}

func newWorkerRegistry() *workerRegistry {
	return &workerRegistry{entries: make(map[string]*model.WorkerInfo)}
}

// Heartbeat stores (or replaces) a worker's latest status snapshot.
func (r *workerRegistry) Heartbeat(info *model.WorkerInfo) {
	info.LastHeartbeat = time.Now()
	r.mu.Lock()
	r.entries[info.ID] = info
	r.mu.Unlock()
}

// List returns every worker whose last heartbeat is still fresh. Stale
// entries are dropped lazily so callers always see a consistent "live" set.
func (r *workerRegistry) List() []*model.WorkerInfo {
	cutoff := time.Now().Add(-workerStaleAfter)
	r.mu.Lock()
	out := make([]*model.WorkerInfo, 0, len(r.entries))
	for id, w := range r.entries {
		if w.LastHeartbeat.Before(cutoff) {
			delete(r.entries, id)
			continue
		}
		out = append(out, w)
	}
	r.mu.Unlock()
	return out
}

// Server owns the store, scheduler and HTTP listener. All fields are set in
// New and read-only afterwards, so Server is safe for concurrent use.
type Server struct {
	store       *store.Store
	sched       *Scheduler
	workers     *workerRegistry
	httpSrv     *http.Server
	token       string       // shared secret; empty disables auth (local-only mode)
	tlsCert     string       // TLS cert path; empty = plain HTTP
	tlsKey      string       // TLS key path; empty = plain HTTP
	auditLogger *slog.Logger // independent audit log (nil = disabled)
	auditFile   *os.File     // underlying audit log file handle; closed in Close()
	rateLimiter *rateLimiter // per-IP token bucket (nil = disabled)
}

// New builds a Server bound to addr. token is the shared secret required by
// API clients and workers; pass "" to run without authentication (only safe
// on a trusted local interface). tlsCert/tlsKey enable HTTPS when both are
// set; pass empty strings for plain HTTP. auditLogger/auditFile are the
// independent audit logger and its underlying file handle (both nil = disabled);
// the caller must open the file and Close will close it. rateLimit is the max
// requests per minute per IP (0 = disabled). Call Start to run the listener.
func New(addr, dbPath string, syncEvery time.Duration, token, tlsCert, tlsKey string, auditLogger *slog.Logger, auditFile *os.File, rateLimit int) (*Server, error) {
	st, err := store.New(dbPath)
	if err != nil {
		return nil, err
	}
	lk := lock.NewMemory()
	sch := NewScheduler(st, lk, syncEvery)
	mux := http.NewServeMux()
	s := &Server{
		store:       st,
		sched:       sch,
		workers:     newWorkerRegistry(),
		token:       token,
		tlsCert:     tlsCert,
		tlsKey:      tlsKey,
		auditLogger: auditLogger,
		auditFile:   auditFile,
	}
	if rateLimit > 0 {
		s.rateLimiter = newRateLimiter(rateLimit)
	}
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
// discoverable in one place. Every /api/* handler is wrapped in a three-layer
// middleware chain: rate limit → auth → audit → handler. UI pages skip the
// audit log but still go through auth (uiAuth, in-page token query param).
func (s *Server) register(mux *http.ServeMux) {
	mux.HandleFunc("/api/tasks", s.rateLimitMiddleware(s.authMiddleware(s.auditMiddleware(s.handleTasks))))
	mux.HandleFunc("/api/tasks/", s.rateLimitMiddleware(s.authMiddleware(s.auditMiddleware(s.handleTaskByID))))
	mux.HandleFunc("/api/pull", s.rateLimitMiddleware(s.authMiddleware(s.auditMiddleware(s.handlePull))))
	mux.HandleFunc("/api/result", s.rateLimitMiddleware(s.authMiddleware(s.auditMiddleware(s.handleResult))))
	mux.HandleFunc("/api/worker/heartbeat", s.rateLimitMiddleware(s.authMiddleware(s.handleWorkerHeartbeat)))
	mux.HandleFunc("/api/workers", s.rateLimitMiddleware(s.authMiddleware(s.handleWorkers)))
	mux.HandleFunc("/tasks/", s.renderTaskDetail)
	mux.HandleFunc("/", s.handleUI)
}

// authMiddleware enforces the shared token on API routes. When no token is
// configured the wrapper is a pass-through so local-only deployments keep
// working without credentials.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && !s.checkToken(r) {
			writeErr(w, http.StatusUnauthorized, "unauthorized: missing or invalid token")
			return
		}
		next(w, r)
	}
}

// checkToken extracts the bearer token from the Authorization header (or the
// X-Token header as a convenience alias) and compares it to the configured
// secret. Uses a constant-time-ish comparison to avoid leaking timing info.
func (s *Server) checkToken(r *http.Request) bool {
	given := ""
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		given = auth[len("Bearer "):]
	} else if t := r.Header.Get("X-Token"); t != "" {
		given = t
	}
	return given != "" && subtle.ConstantTimeCompare([]byte(given), []byte(s.token)) == 1
}

// uiAuth decides whether a UI page request is allowed. In addition to the
// bearer header it accepts a ?token= query parameter so users can open the
// dashboard directly from a browser address bar.
func (s *Server) uiAuth(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	if s.checkToken(r) {
		return true
	}
	return r.URL.Query().Get("token") == s.token
}

// Start runs the scheduler and HTTP listener until ctx is cancelled.
// When tlsCert/tlsKey are configured it serves HTTPS, otherwise plain HTTP.
// Also starts the rateLimiter cleanup goroutine if rate limiting is enabled.
// Shutdown blocks in-progress requests up to 5s then returns.
func (s *Server) Start(ctx context.Context) error {
	go s.sched.Start(ctx)
	if s.rateLimiter != nil {
		// Clean stale rate-limit entries every 5 min; IPs idle for >2h get evicted.
		go s.rateLimiter.runCleanupPeriodically(ctx, 5*time.Minute, 2*time.Hour)
	}
	if s.tlsCert != "" && s.tlsKey != "" {
		logger.L.Info("server listening (TLS)", "addr", s.httpSrv.Addr)
		err := s.httpSrv.ListenAndServeTLS(s.tlsCert, s.tlsKey)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
	logger.L.Info("server listening (plain HTTP)", "addr", s.httpSrv.Addr)
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

// Close releases all resources: the store and the audit log file handle.
// Safe to call once at shutdown. Audit log errors are logged but don't
// mask a store close error since that's the more important resource.
func (s *Server) Close() error {
	var err error
	if s.auditFile != nil {
		if cerr := s.auditFile.Close(); cerr != nil {
			logger.L.Warn("audit file close failed", "err", cerr)
		}
	}
	if serr := s.store.Close(); serr != nil {
		err = serr
	}
	return err
}

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
	if t == model.ScheduleCron {
		parser := cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
		if _, err := parser.Parse(req.CronExpr); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid cron_expr: "+err.Error())
			return
		}
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
	// If it's a cron definition, resync the scheduler immediately so the
	// first spawn doesn't wait a full syncEvery interval.
	if task.Type == model.ScheduleCron {
		s.sched.ResyncNow(r.Context())
	}
	writeJSON(w, http.StatusCreated, task)
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
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
		Status:   q.Get("status"),
		Worker:   q.Get("worker"),
		Enabled:  q.Get("enabled"),
		ParentID: parseParentID(q.Get("parent_id")),
	}
	offset := (page - 1) * pageSize
	tasks, err := s.store.List(r.Context(), f, offset, pageSize)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	total, err := s.store.Count(r.Context(), f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	totalPages := int(total) / pageSize
	if int(total)%pageSize != 0 {
		totalPages++
	}
	if totalPages < 1 {
		totalPages = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":       tasks,
		"total":       total,
		"page":        page,
		"page_size":   pageSize,
		"total_pages": totalPages,
	})
}

// parseParentID converts the parent_id query param to int64. Invalid or empty
// values yield 0 (the "top-level tasks only" default).
func parseParentID(s string) int64 {
	if s == "" {
		return 0
	}
	id, _ := strconv.ParseInt(s, 10, 64)
	return id
}

// handleTaskByID routes per-task operations. Path forms supported:
//
//	GET    /api/tasks/{id}          fetch details
//	DELETE /api/tasks/{id}          remove the task row
//	POST   /api/tasks/{id}/pause   disable a cron definition (stops spawns)
//	POST   /api/tasks/{id}/resume  re-enable a paused cron definition
//	POST   /api/tasks/{id}/stop    interrupt a running/pending task instance
//	POST   /api/tasks/{id}/restart resume a stopped task (back to pending)
//
// ServeMux matches /api/tasks/ as a subtree, so /api/tasks/1/pause lands here
// too; we split on '/' to read the optional action.
func (s *Server) handleTaskByID(w http.ResponseWriter, r *http.Request) {
	// Path after "/api/tasks/": either "{id}" or "{id}/{action}".
	rest := r.URL.Path[len("/api/tasks/"):]
	parts := []string{}
	for _, p := range strings.Split(rest, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 || len(parts) > 2 {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid id")
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	// Dispatch on (method, action).
	switch {
	case r.Method == http.MethodGet && action == "":
		s.getTask(w, r, id)
	case r.Method == http.MethodDelete && action == "":
		s.deleteTask(w, r, id)
	case r.Method == http.MethodPost && action == "pause":
		s.setTaskEnabled(w, r, id, false)
	case r.Method == http.MethodPost && action == "resume":
		s.setTaskEnabled(w, r, id, true)
	case r.Method == http.MethodPost && action == "stop":
		s.stopTask(w, r, id)
	case r.Method == http.MethodPost && action == "restart":
		s.resumeTask(w, r, id)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request, id int64) {
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

func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.store.Delete(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) setTaskEnabled(w http.ResponseWriter, r *http.Request, id int64, enabled bool) {
	if err := s.store.SetEnabled(r.Context(), id, enabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "task not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Nudge the scheduler to resync now so pause/resume takes effect at once
	// instead of waiting up to syncEvery (default 30s).
	s.sched.ResyncNow(r.Context())
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) stopTask(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.store.StopTask(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "task not found or already stopped")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) resumeTask(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.store.ResumeTask(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "task not found or not in stopped state")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handlePull is the worker's main entry: atomically claim the next due
// pending task. The response always includes a StopList of task ids the
// worker owns that have been marked stopped — the worker must cancel those
// in-flight processes. When there is no task to run the task field is null.
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
	resp := model.PullResponse{}
	t, err := s.store.ClaimPending(r.Context(), workerID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if t != nil {
		resp.Task = t
	}
	// Always attach the stop list so the worker can cancel in-flight tasks
	// that were stopped while executing.
	stopList, err := s.store.GetStopList(r.Context(), workerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp.StopList = stopList
	writeJSON(w, http.StatusOK, resp)
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

// handleWorkerHeartbeat accepts a worker's status snapshot and stores it in
// the in-memory registry. Workers should call this every few seconds.
func (s *Server) handleWorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var info model.WorkerInfo
	if err := json.NewDecoder(r.Body).Decode(&info); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if info.ID == "" {
		writeErr(w, http.StatusBadRequest, "id required")
		return
	}
	s.workers.Heartbeat(&info)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleWorkers returns the list of currently live (recently heartbeating)
// workers for the dashboard.
func (s *Server) handleWorkers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	writeJSON(w, http.StatusOK, s.workers.List())
}

// handleUI renders the simple task dashboard. Implemented in ui.go.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	s.renderUI(w, r)
}

// ---- P2-2 审计日志 ----------------------------------------------------------

// captureWriter wraps http.ResponseWriter to capture the final status code
// written by the downstream handler. auditMiddleware uses it to log the
// outcome after the handler returns.
type captureWriter struct {
	http.ResponseWriter
	status int
}

func newCaptureWriter(w http.ResponseWriter) *captureWriter {
	return &captureWriter{ResponseWriter: w, status: http.StatusOK}
}

func (cw *captureWriter) WriteHeader(code int) {
	cw.status = code
	cw.ResponseWriter.WriteHeader(code)
}

// clientIP extracts the best-guess client IP from a request, respecting
// X-Forwarded-For headers (first non-trusted entry) and falling back to
// RemoteAddr. Stripped of port for cleaner logs.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if idx := strings.IndexByte(xff, ','); idx >= 0 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if host == "" {
		return r.RemoteAddr
	}
	return host
}

// auditMiddleware wraps a handler and writes a structured audit record after
// it returns. Fields: time, client IP, method, path, task_id (when present),
// and HTTP status. Skipped when auditLogger is nil.
func (s *Server) auditMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.auditLogger == nil {
			next(w, r)
			return
		}
		cw := newCaptureWriter(w)
		next(cw, r)

		// Extract task id from paths like /api/tasks/123 or /api/tasks/123/pause
		taskID := ""
		if strings.HasPrefix(r.URL.Path, "/api/tasks/") {
			parts := strings.Split(r.URL.Path[len("/api/tasks/"):], "/")
			if len(parts) > 0 {
				taskID = parts[0]
			}
		}

		s.auditLogger.Info("audit",
			"ts", time.Now().UTC().Format(time.RFC3339),
			"remote_ip", clientIP(r),
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"task_id", taskID,
			"status", cw.status,
		)
	}
}

// ---- P3-1 速率限制 ----------------------------------------------------------

// rateLimiter is a tiny in-memory per-client token bucket. Clients that
// exhaust their tokens get 429 Too Many Requests. It's intentionally simple
// — no external state, no precise fairness — good enough for a lightweight
// scheduler where DoS risk is modest.
type rateLimiter struct {
	mu     sync.Mutex
	tokens map[string]float64
	last   map[string]time.Time
	perMin float64 // capacity + refill rate
}

func newRateLimiter(perMin int) *rateLimiter {
	return &rateLimiter{
		tokens: make(map[string]float64),
		last:   make(map[string]time.Time),
		perMin: float64(perMin),
	}
}

// allow returns true if the client identified by key has a token available.
// A request consumes one token; tokens refill linearly over time up to the
// perMin ceiling.
func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	tokens, ok := rl.tokens[key]
	if !ok {
		tokens = rl.perMin // fresh client starts with full bucket
	} else {
		elapsed := now.Sub(rl.last[key]).Minutes()
		tokens += elapsed * rl.perMin
		if tokens > rl.perMin {
			tokens = rl.perMin
		}
	}
	if tokens < 1 {
		rl.last[key] = now
		rl.tokens[key] = tokens
		return false
	}
	tokens--
	rl.last[key] = now
	rl.tokens[key] = tokens
	return true
}

// cleanup removes stale entries whose last access is older than the given
// TTL. Prevents unbounded growth of the per-client maps on long-running
// servers. Called periodically from a goroutine in Server.Start.
func (rl *rateLimiter) cleanup(ttl time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := time.Now().Add(-ttl)
	for k, t := range rl.last {
		if t.Before(cutoff) {
			delete(rl.tokens, k)
			delete(rl.last, k)
		}
	}
}

// runCleanupPeriodically calls cleanup every interval until ctx is cancelled.
func (rl *rateLimiter) runCleanupPeriodically(ctx context.Context, interval, ttl time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rl.cleanup(ttl)
		}
	}
}

// rateLimitMiddleware returns 429 if the client IP has exceeded the per-minute
// request budget. Disabled when rateLimiter is nil.
func (s *Server) rateLimitMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.rateLimiter != nil && !s.rateLimiter.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next(w, r)
	}
}
