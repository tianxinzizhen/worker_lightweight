// Package worker implements the pull-based worker. A single fetcher goroutine
// polls the server for due tasks and feeds them into a buffered jobs channel;
// a fixed pool of executor goroutines drains that channel, runs each task
// with a timeout, and reports the outcome back to the server.
//
// The buffered channel is the concurrency control: when all executors are
// busy, the fetcher blocks on send, which is the desired backpressure.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/tianxinzizhen/worker_lightweight/internal/logger"
	"github.com/tianxinzizhen/worker_lightweight/internal/model"
)

// Config tunes the worker's behaviour.
type Config struct {
	ID          string        // unique worker identifier reported with results
	ServerAddr  string        // e.g. http://127.0.0.1:8080
	Workers     int           // executor pool size
	PullEvery   time.Duration // fetcher poll interval (rate limiting)
	HTTPTimeout time.Duration
	Token       string // shared secret sent as Authorization bearer; "" disables
}

// Pool ties together the fetcher, executors and HTTP client.
type Pool struct {
	cfg       Config
	client    *http.Client
	jobs      chan *model.Task
	startedAt time.Time

	// active tracks in-flight tasks so the fetcher can cancel them when the
	// server reports a stop. The mutex guards the map only; cancelling a
	// context is safe to do without holding it.
	active   map[int64]context.CancelFunc
	activeMu sync.Mutex
}

// NewPool constructs a Pool. The jobs channel is buffered to workers*2 so the
// fetcher can keep a small queue warm without blocking on every poll.
func NewPool(cfg Config) *Pool {
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 10 * time.Second
	}
	return &Pool{
		cfg:       cfg,
		client:    &http.Client{Timeout: cfg.HTTPTimeout},
		jobs:      make(chan *model.Task, cfg.Workers*2),
		active:    make(map[int64]context.CancelFunc),
		startedAt: time.Now(),
	}
}

// registerActive stores the cancel func for a task that just started. Called
// by runTask before executing the command.
func (p *Pool) registerActive(id int64, cancel context.CancelFunc) {
	p.activeMu.Lock()
	p.active[id] = cancel
	p.activeMu.Unlock()
}

// unregisterActive removes a task's cancel func. Called after runTask finishes
// (whether success, failure, or cancellation).
func (p *Pool) unregisterActive(id int64) {
	p.activeMu.Lock()
	delete(p.active, id)
	p.activeMu.Unlock()
}

// activeSnapshot returns a copy of the currently in-flight task ids. Used by
// the heartbeat reporter to tell the server which tasks this worker owns.
func (p *Pool) activeSnapshot() []int64 {
	p.activeMu.Lock()
	ids := make([]int64, 0, len(p.active))
	for id := range p.active {
		ids = append(ids, id)
	}
	p.activeMu.Unlock()
	return ids
}

// cancelActive stops a task's execution context if it is currently running.
// Returns true if the task was active and got cancelled. Removes the entry
// immediately so repeated stop-list polls don't log duplicate cancels.
func (p *Pool) cancelActive(id int64) bool {
	p.activeMu.Lock()
	cancel, ok := p.active[id]
	if ok {
		delete(p.active, id)
	}
	p.activeMu.Unlock()
	if ok {
		cancel()
		return true
	}
	return false
}

// Start launches the fetcher and executor goroutines and blocks until ctx is
// cancelled. On exit it waits for in-flight tasks to finish (drain jobs).
func (p *Pool) Start(ctx context.Context) {
	// executors: read from jobs until the channel closes.
	done := make(chan struct{})
	for i := 0; i < p.cfg.Workers; i++ {
		go func(id int) {
			defer func() { done <- struct{}{} }()
			for {
				select {
				case <-ctx.Done():
					return
				case t, ok := <-p.jobs:
					if !ok {
						return
					}
					p.runTask(ctx, id, t)
				}
			}
		}(i)
	}

	// fetcher: poll until ctx cancelled, then close jobs so executors exit.
	go func() {
		t := time.NewTicker(p.cfg.PullEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				close(p.jobs)
				return
			case <-t.C:
				p.fetch(ctx)
			}
		}
	}()

	// heartbeat: report worker status to the server every few seconds so the
	// dashboard can show live pool utilisation.
	go func() {
		// Send one immediately so the worker appears without waiting a cycle.
		p.heartbeat(ctx)
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.heartbeat(ctx)
			}
		}
	}()

	// Wait for all executors to drain remaining jobs and exit.
	for i := 0; i < p.cfg.Workers; i++ {
		<-done
	}
	logger.L.Info("worker pool stopped", "id", p.cfg.ID)
}

// setAuth attaches the shared secret to an outgoing request when configured.
// The server's authMiddleware rejects requests without a valid bearer token.
func (p *Pool) setAuth(req *http.Request) {
	if p.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	}
}

// fetch asks the server for one due task. The response is a PullResponse
// that always carries a StopList of task ids the worker owns that have been
// marked stopped — we cancel those in-flight processes immediately. If the
// task field is null there is nothing to run this cycle.
func (p *Pool) fetch(ctx context.Context) {
	url := fmt.Sprintf("%s/api/pull?worker_id=%s", p.cfg.ServerAddr, p.cfg.ID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	p.setAuth(req)
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			logger.L.Warn("worker pull failed", "err", err)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.L.Warn("worker pull bad status", "status", resp.Status)
		return
	}
	var pr model.PullResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		logger.L.Warn("worker pull decode failed", "err", err)
		return
	}
	// Cancel any in-flight tasks the server says have been stopped.
	for _, id := range pr.StopList {
		if p.cancelActive(id) {
			logger.L.Info("worker cancelling stopped task", "id", id, "worker", p.cfg.ID)
		}
	}
	if pr.Task == nil {
		return // idle, expected
	}
	select {
	case p.jobs <- pr.Task:
		logger.L.Info("worker fetched task", "id", pr.Task.ID, "name", pr.Task.Name, "worker", p.cfg.ID)
	case <-ctx.Done():
	}
}

// runTask executes the claimed task and reports the result. The timeout
// context is derived from the task config; 0 means unbounded (rare).
func (p *Pool) runTask(parent context.Context, workerNum int, t *model.Task) {
	start := time.Now()
	logger.L.Info("executing task", "id", t.ID, "name", t.Name, "worker", p.cfg.ID, "slot", workerNum)

	ctx, cancel := context.WithTimeout(parent, p.taskTimeout(t))
	// Register so the fetcher can cancel this context if a stop arrives.
	p.registerActive(t.ID, cancel)
	defer func() {
		p.unregisterActive(t.ID)
		cancel()
	}()

	success, output := p.exec(ctx, t)
	// Cancellation is reported as a failure so the server keeps the stopped
	// state; ReportResult detects the stopped row and leaves it as-is.
	if ctx.Err() != nil && success {
		success = false
		if output == "" {
			output = "stopped by user"
		}
	}
	logger.L.Info("task finished",
		"id", t.ID, "success", success, "dur_ms", time.Since(start).Milliseconds(),
		"worker", p.cfg.ID, "slot", workerNum)

	p.report(ctx, t.ID, success, output)
}

func (p *Pool) taskTimeout(t *model.Task) time.Duration {
	if t.TimeoutSec <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(t.TimeoutSec) * time.Second
}

// exec runs the command via /bin/sh -c so users can use pipes, &&, etc.
// On context deadline the process is killed by CommandContext. We capture
// both stdout and stderr: stdout on success, stderr on failure. Truncating
// to a sane size keeps the result column readable in the UI.
func (p *Pool) exec(ctx context.Context, t *model.Task) (bool, string) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", t.Command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, truncStr(stdout.String(), 4096)
	}
	// Deadline exceeded counts as failure (caller retries per MaxRetry).
	msg := stderr.String()
	if msg == "" {
		msg = err.Error()
	}
	return false, truncStr(msg, 4096)
}

// report sends the execution outcome to the server. Failures here only mean
// the task stays 'running' in the store; the next server restart sweep would
// recover it. We log but don't retry to avoid masking the original result.
func (p *Pool) report(ctx context.Context, taskID int64, success bool, output string) {
	body, _ := json.Marshal(model.Result{
		TaskID:   taskID,
		WorkerID: p.cfg.ID,
		Success:  success,
		Output:   output,
	})
	url := p.cfg.ServerAddr + "/api/result"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	p.setAuth(req)
	resp, err := p.client.Do(req)
	if err != nil {
		logger.L.Error("report result failed", "id", taskID, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		logger.L.Error("report result bad status", "id", taskID, "status", resp.Status, "body", string(b))
	}
}

// heartbeat sends the worker's current status (pool size, active count,
// running task ids, uptime) to the server. Failures are non-fatal: the next
// tick will retry, and the server marks stale workers offline automatically.
func (p *Pool) heartbeat(ctx context.Context) {
	active := p.activeSnapshot()
	body, _ := json.Marshal(model.WorkerInfo{
		ID:           p.cfg.ID,
		TotalSlots:   p.cfg.Workers,
		ActiveCount:  len(active),
		RunningTasks: active,
		StartedAt:    p.startedAt,
	})
	url := p.cfg.ServerAddr + "/api/worker/heartbeat"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	p.setAuth(req)
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			logger.L.Warn("worker heartbeat failed", "err", err)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.L.Warn("worker heartbeat bad status", "status", resp.Status)
	}
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
