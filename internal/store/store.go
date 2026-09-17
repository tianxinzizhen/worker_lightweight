// Package store persists tasks to SQLite. The Claim method uses an atomic
// UPDATE ... WHERE status='pending' so two concurrent workers can never grab
// the same task instance — the database is the source of truth for ownership.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/tianxinzizhen/worker_lightweight/internal/model"
)

// ErrNotFound is returned when a task id does not exist.
var ErrNotFound = errors.New("task not found")

// Store wraps the database handle. All methods are safe for concurrent use
// because database/sql manages its own connection pool and the driver
// serializes writes via SQLite's file lock.
type Store struct {
	db *sql.DB
}

// New opens (or creates) the SQLite file, applies the schema, and tunes
// connection-pool sizes. WAL mode greatly improves write concurrency over
// the default journal, which matters when many workers poll simultaneously.
func New(path string) (*Store, error) {
	// busy_timeout lets writers wait briefly instead of failing when
	// another connection holds the lock; _pragma checks run on every open.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite thrives on a small pool; more connections just increase lock
	// contention. 1 writer connection is the safest default.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close releases the database handle. Safe to call once at shutdown.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const ddl = `
CREATE TABLE IF NOT EXISTS tasks (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL,
    command      TEXT    NOT NULL,
    type         TEXT    NOT NULL,
    cron_expr    TEXT    NOT NULL DEFAULT '',
    status       TEXT    NOT NULL,
    max_retry    INTEGER NOT NULL DEFAULT 0,
    retry_count INTEGER NOT NULL DEFAULT 0,
    timeout_sec  INTEGER NOT NULL DEFAULT 0,
    worker_id    TEXT    NOT NULL DEFAULT '',
    result       TEXT    NOT NULL DEFAULT '',
    created_at   DATETIME NOT NULL,
    updated_at   DATETIME NOT NULL,
    next_run_at  DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tasks_status_next ON tasks(status, next_run_at);
CREATE INDEX IF NOT EXISTS idx_tasks_type       ON tasks(type);
`
	_, err := s.db.Exec(ddl)
	return err
}

// Create inserts a new task row. The caller is responsible for setting
// ScheduleType; cron tasks get next_run_at=created_at so they fire on the
// first matching tick rather than immediately.
func (s *Store) Create(ctx context.Context, t *model.Task) error {
	now := time.Now()
	t.CreatedAt = now
	t.UpdatedAt = now
	if t.NextRunAt.IsZero() {
		t.NextRunAt = now
	}
	res, err := s.db.ExecContext(ctx, `
INSERT INTO tasks(name, command, type, cron_expr, status, max_retry, retry_count, timeout_sec, worker_id, result, created_at, updated_at, next_run_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.Name, t.Command, t.Type, t.CronExpr, t.Status, t.MaxRetry, t.RetryCount,
		t.TimeoutSec, t.WorkerID, t.Result, t.CreatedAt, t.UpdatedAt, t.NextRunAt)
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	t.ID, _ = res.LastInsertId()
	return nil
}

// Get fetches a single task by id. Returns ErrNotFound when absent.
func (s *Store) Get(ctx context.Context, id int64) (*model.Task, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, name, command, type, cron_expr, status, max_retry, retry_count, timeout_sec, worker_id, result, created_at, updated_at, next_run_at
FROM tasks WHERE id=?`, id)
	t := &model.Task{}
	err := row.Scan(&t.ID, &t.Name, &t.Command, &t.Type, &t.CronExpr, &t.Status,
		&t.MaxRetry, &t.RetryCount, &t.TimeoutSec, &t.WorkerID, &t.Result,
		&t.CreatedAt, &t.UpdatedAt, &t.NextRunAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// List returns up to limit tasks ordered newest-first. Used by the web UI.
func (s *Store) List(ctx context.Context, limit int) ([]*model.Task, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, command, type, cron_expr, status, max_retry, retry_count, timeout_sec, worker_id, result, created_at, updated_at, next_run_at
FROM tasks ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Task
	for rows.Next() {
		t := &model.Task{}
		if err := rows.Scan(&t.ID, &t.Name, &t.Command, &t.Type, &t.CronExpr,
			&t.Status, &t.MaxRetry, &t.RetryCount, &t.TimeoutSec, &t.WorkerID,
			&t.Result, &t.CreatedAt, &t.UpdatedAt, &t.NextRunAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListCron returns the definition rows the scheduler iterates over. Cron rows
// are status=success (the template ran once conceptually) — actually we keep
// them in 'pending' forever and never claim them; see ClaimPending.
func (s *Store) ListCron(ctx context.Context) ([]*model.Task, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, command, type, cron_expr, status, max_retry, retry_count, timeout_sec, worker_id, result, created_at, updated_at, next_run_at
FROM tasks WHERE type=?`, model.ScheduleCron)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Task
	for rows.Next() {
		t := &model.Task{}
		if err := rows.Scan(&t.ID, &t.Name, &t.Command, &t.Type, &t.CronExpr,
			&t.Status, &t.MaxRetry, &t.RetryCount, &t.TimeoutSec, &t.WorkerID,
			&t.Result, &t.CreatedAt, &t.UpdatedAt, &t.NextRunAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ClaimPending atomically transitions the oldest due pending task to running
// and assigns it to workerID. The WHERE status='pending' clause is the
// database-level lock that guarantees a task is claimed by at most one
// worker even under concurrent ClaimPending calls. Returns ErrNotFound when
// no task is eligible (a normal condition, not an error to the caller).
func (s *Store) ClaimPending(ctx context.Context, workerID string) (*model.Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `
SELECT id, name, command, type, cron_expr, status, max_retry, retry_count, timeout_sec, worker_id, result, created_at, updated_at, next_run_at
FROM tasks
WHERE type=? AND status=? AND next_run_at<=?
ORDER BY next_run_at ASC
LIMIT 1`, model.ScheduleOnce, model.StatusPending, time.Now())
	t := &model.Task{}
	err = row.Scan(&t.ID, &t.Name, &t.Command, &t.Type, &t.CronExpr, &t.Status,
		&t.MaxRetry, &t.RetryCount, &t.TimeoutSec, &t.WorkerID, &t.Result,
		&t.CreatedAt, &t.UpdatedAt, &t.NextRunAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// Atomic state transition: only flips the row if still pending.
	res, err := tx.ExecContext(ctx, `
UPDATE tasks SET status=?, worker_id=?, updated_at=? WHERE id=? AND status=?`,
		model.StatusRunning, workerID, time.Now(), t.ID, model.StatusPending)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Lost the race — another worker grabbed it first.
		return nil, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	t.Status = model.StatusRunning
	t.WorkerID = workerID
	return t, nil
}

// ReportResult records the worker's outcome. On failure with retries left it
// flips the task back to pending (the next worker poll will retry it) and
// bumps retry_count; otherwise the task is marked dead/success permanently.
func (s *Store) ReportResult(ctx context.Context, r *model.Result) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var status, workerID string
	var retryCount, maxRetry int
	err = tx.QueryRowContext(ctx, `SELECT status, worker_id, retry_count, max_retry FROM tasks WHERE id=?`, r.TaskID).
		Scan(&status, &workerID, &retryCount, &maxRetry)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// Only the worker that owns the task may report its result. This guards
	// against stale or duplicate reports after a failover.
	if workerID != r.WorkerID {
		return fmt.Errorf("worker %s not owner of task %d (owner=%s)", r.WorkerID, r.TaskID, workerID)
	}

	now := time.Now()
	newRetry := retryCount
	if !r.Success {
		newRetry = retryCount + 1
	}

	var newStatus model.TaskStatus
	switch {
	case r.Success:
		newStatus = model.StatusSuccess
	case newRetry > maxRetry:
		newStatus = model.StatusDead
	default:
		// Back to pending so the retry loop re-claims it. Adding a small
		// delay via next_run_at thunders batches of failing tasks.
		newStatus = model.StatusPending
	}

	if newStatus == model.StatusPending {
		_, err = tx.ExecContext(ctx, `
UPDATE tasks SET status=?, retry_count=?, result=?, worker_id='', updated_at=?, next_run_at=?
WHERE id=? AND status=?`,
			newStatus, newRetry, r.Output, now,
			now.Add(time.Duration(newRetry)*5*time.Second),
			r.TaskID, model.StatusRunning)
	} else {
		_, err = tx.ExecContext(ctx, `
UPDATE tasks SET status=?, retry_count=?, result=?, worker_id=?, updated_at=? WHERE id=?`,
			newStatus, newRetry, r.Output, r.WorkerID, now, r.TaskID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
