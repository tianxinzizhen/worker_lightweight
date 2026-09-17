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
}

// Pool ties together the fetcher, executors and HTTP client.
type Pool struct {
	cfg    Config
	client *http.Client
	jobs   chan *model.Task
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
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.HTTPTimeout},
		jobs:   make(chan *model.Task, cfg.Workers*2),
	}
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

	// Wait for all executors to drain remaining jobs and exit.
	for i := 0; i < p.cfg.Workers; i++ {
		<-done
	}
	logger.L.Info("worker pool stopped", "id", p.cfg.ID)
}

// fetch asks the server for one due task. 204 means idle (normal). On success
// the task is handed to the jobs channel; if ctx is cancelled mid-send the
// task is dropped — but it remains in 'running' status in the store and will
// be re-queued by a future recovery sweep (TODO: add a stuck-task sweeper).
func (p *Pool) fetch(ctx context.Context) {
	url := fmt.Sprintf("%s/api/pull?worker_id=%s", p.cfg.ServerAddr, p.cfg.ID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			logger.L.Warn("worker pull failed", "err", err)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return // idle, expected
	}
	if resp.StatusCode != http.StatusOK {
		logger.L.Warn("worker pull bad status", "status", resp.Status)
		return
	}
	var t model.Task
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		logger.L.Warn("worker pull decode failed", "err", err)
		return
	}
	select {
	case p.jobs <- &t:
		logger.L.Info("worker fetched task", "id", t.ID, "name", t.Name, "worker", p.cfg.ID)
	case <-ctx.Done():
	}
}

// runTask executes the claimed task and reports the result. The timeout
// context is derived from the task config; 0 means unbounded (rare).
func (p *Pool) runTask(parent context.Context, workerNum int, t *model.Task) {
	start := time.Now()
	logger.L.Info("executing task", "id", t.ID, "name", t.Name, "worker", p.cfg.ID, "slot", workerNum)

	ctx, cancel := context.WithTimeout(parent, p.taskTimeout(t))
	defer cancel()

	success, output := p.exec(ctx, t)
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

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
