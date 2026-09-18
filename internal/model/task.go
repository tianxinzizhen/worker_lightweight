// Package model defines the cross-cutting data structures exchanged between
// the server and worker. Keeping them in one package avoids import cycles.
package model

import (
	"encoding/json"
	"time"
)

// TaskStatus enumerates the lifecycle states a task instance may be in.
// Transitions: Pending -> Running -> (Success | Failed -> Pending on retry).
type TaskStatus string

const (
	StatusPending TaskStatus = "pending"
	StatusRunning TaskStatus = "running"
	StatusSuccess TaskStatus = "success"
	StatusFailed  TaskStatus = "failed"
	StatusDead    TaskStatus = "dead"    // exhausted retries, will not run again
	StatusStopped TaskStatus = "stopped" // user stopped; can be resumed back to pending
)

// ScheduleType distinguishes one-shot tasks from cron-triggered recurring tasks.
type ScheduleType string

const (
	ScheduleOnce ScheduleType = "once"
	ScheduleCron ScheduleType = "cron"
)

// Task is the persisted definition of a unit of work. The same row serves as
// both the template (for cron) and the executable instance (for once). The
// server never mutates a cron row's status past pending; each cron tick
// creates a fresh child Task so retries stay isolated.
type Task struct {
	ID         int64        `json:"id"`
	Name       string       `json:"name"`
	Command    string       `json:"command"` // shell command the worker runs
	Type       ScheduleType `json:"type"`    // once | cron
	CronExpr   string       `json:"cron_expr,omitempty"`
	Status     TaskStatus   `json:"status"`
	MaxRetry   int          `json:"max_retry"`
	RetryCount int          `json:"retry_count"`
	TimeoutSec int          `json:"timeout_sec"`         // 0 means no timeout
	WorkerID   string       `json:"worker_id,omitempty"` // who claimed it
	ParentID   int64        `json:"parent_id,omitempty"` // 0 for top-level; >0 for cron-spawned children
	Enabled    bool         `json:"enabled"`             // false pauses cron spawns; new rows default true via store.Create
	Result     string       `json:"result,omitempty"`    // stdout / error message
	CreatedAt  time.Time    `json:"created_at"`
	UpdatedAt  time.Time    `json:"updated_at"`
	NextRunAt  time.Time    `json:"next_run_at"` // when the task becomes eligible
}

// MarshalJSON renders timestamps as RFC3339 (no sub-second precision) so the
// JSON API and the HTML template — which uses t.CreatedAt.Format(time.RFC3339)
// in ui.go — display identical human-readable formats. Without this override,
// time.Time's default marshaling emits RFC3339Nano (e.g.
// "2026-09-17T19:46:15.123456789+08:00") and the dashboard shows two different
// formats between the initial server render and the JS-driven local refresh.
// jsonTask is a same-shape struct with string timestamp fields, breaking the
// recursion that would otherwise re-enter Task.MarshalJSON.
func (t Task) MarshalJSON() ([]byte, error) {
	return json.Marshal(jsonTask{
		ID:         t.ID,
		Name:       t.Name,
		Command:    t.Command,
		Type:       t.Type,
		CronExpr:   t.CronExpr,
		Status:     t.Status,
		MaxRetry:   t.MaxRetry,
		RetryCount: t.RetryCount,
		TimeoutSec: t.TimeoutSec,
		WorkerID:   t.WorkerID,
		ParentID:   t.ParentID,
		Enabled:    t.Enabled,
		Result:     t.Result,
		CreatedAt:  t.CreatedAt.Format(time.RFC3339),
		UpdatedAt:  t.UpdatedAt.Format(time.RFC3339),
		NextRunAt:  t.NextRunAt.Format(time.RFC3339),
	})
}

// jsonTask mirrors Task but replaces time.Time with string so the MarshalJSON
// override can emit RFC3339-formatted timestamps instead of time.Time's
// default RFC3339Nano serialization.
type jsonTask struct {
	ID         int64        `json:"id"`
	Name       string       `json:"name"`
	Command    string       `json:"command"`
	Type       ScheduleType `json:"type"`
	CronExpr   string       `json:"cron_expr,omitempty"`
	Status     TaskStatus   `json:"status"`
	MaxRetry   int          `json:"max_retry"`
	RetryCount int          `json:"retry_count"`
	TimeoutSec int          `json:"timeout_sec"`
	WorkerID   string       `json:"worker_id,omitempty"`
	ParentID   int64        `json:"parent_id,omitempty"`
	Enabled    bool         `json:"enabled"`
	Result     string       `json:"result,omitempty"`
	CreatedAt  string       `json:"created_at"`
	UpdatedAt  string       `json:"updated_at"`
	NextRunAt  string       `json:"next_run_at"`
}

// Result is the payload a worker sends back after execution.
type Result struct {
	TaskID   int64  `json:"task_id"`
	WorkerID string `json:"worker_id"`
	Success  bool   `json:"success"`
	Output   string `json:"output"` // stdout on success, stderr/error on failure
}

// PullResponse is the server's reply to a worker pull. Task is nil when
// there is nothing to run (204). StopList contains task ids the worker is
// currently executing that have been marked stopped by the user — the worker
// must cancel those in-flight processes.
type PullResponse struct {
	Task     *Task   `json:"task,omitempty"`
	StopList []int64 `json:"stop_list,omitempty"`
}

// CreateTaskRequest is the JSON body for POST /api/tasks.
type CreateTaskRequest struct {
	Name       string `json:"name"`
	Command    string `json:"command"`
	Type       string `json:"type"`      // "once" or "cron"
	CronExpr   string `json:"cron_expr"` // required when type == cron
	MaxRetry   int    `json:"max_retry"`
	TimeoutSec int    `json:"timeout_sec"`
}

// WorkerInfo is the status payload a worker reports to the server on each
// heartbeat. The server keeps the latest snapshot per worker id so the
// dashboard can show pool utilisation.
type WorkerInfo struct {
	ID            string    `json:"id"`
	TotalSlots    int       `json:"total_slots"`
	ActiveCount   int       `json:"active_count"`
	RunningTasks  []int64   `json:"running_tasks"`
	StartedAt     time.Time `json:"started_at"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
}
