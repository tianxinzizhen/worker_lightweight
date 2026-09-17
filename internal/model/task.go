// Package model defines the cross-cutting data structures exchanged between
// the server and worker. Keeping them in one package avoids import cycles.
package model

import "time"

// TaskStatus enumerates the lifecycle states a task instance may be in.
// Transitions: Pending -> Running -> (Success | Failed -> Pending on retry).
type TaskStatus string

const (
	StatusPending  TaskStatus = "pending"
	StatusRunning  TaskStatus = "running"
	StatusSuccess  TaskStatus = "success"
	StatusFailed   TaskStatus = "failed"
	StatusDead     TaskStatus = "dead" // exhausted retries, will not run again
)

// ScheduleType distinguishes one-shot tasks from cron-triggered recurring tasks.
type ScheduleType string

const (
	ScheduleOnce  ScheduleType = "once"
	ScheduleCron  ScheduleType = "cron"
)

// Task is the persisted definition of a unit of work. The same row serves as
// both the template (for cron) and the executable instance (for once). The
// server never mutates a cron row's status past pending; each cron tick
// creates a fresh child Task so retries stay isolated.
type Task struct {
	ID          int64       `json:"id"`
	Name        string      `json:"name"`
	Command     string      `json:"command"` // shell command the worker runs
	Type        ScheduleType `json:"type"`   // once | cron
	CronExpr    string      `json:"cron_expr,omitempty"`
	Status      TaskStatus  `json:"status"`
	MaxRetry    int         `json:"max_retry"`
	RetryCount  int         `json:"retry_count"`
	TimeoutSec  int         `json:"timeout_sec"` // 0 means no timeout
	WorkerID    string      `json:"worker_id,omitempty"` // who claimed it
	Result      string      `json:"result,omitempty"`    // stdout / error message
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
	NextRunAt   time.Time   `json:"next_run_at"` // when the task becomes eligible
}

// Result is the payload a worker sends back after execution.
type Result struct {
	TaskID   int64  `json:"task_id"`
	WorkerID string `json:"worker_id"`
	Success  bool   `json:"success"`
	Output   string `json:"output"` // stdout on success, stderr/error on failure
}

// CreateTaskRequest is the JSON body for POST /api/tasks.
type CreateTaskRequest struct {
	Name       string `json:"name"`
	Command    string `json:"command"`
	Type       string `json:"type"`        // "once" or "cron"
	CronExpr   string `json:"cron_expr"`   // required when type == cron
	MaxRetry   int    `json:"max_retry"`
	TimeoutSec int    `json:"timeout_sec"`
}
