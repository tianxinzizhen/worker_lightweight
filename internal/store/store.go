// Package store persists tasks to SQLite. The Claim method uses an atomic
// UPDATE ... WHERE status='pending' so two concurrent workers can never grab
// the same task instance — the database is the source of truth for ownership.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// P2-3: lock down permissions so only the owner can read the database
	// and its WAL/SHM siblings — these contain task commands and results.
	secureDB(path)
	return s, nil
}

// secureDB chmods the SQLite file, its -wal/-shm siblings (may not yet
// exist), and the parent directory to owner-only access. Failure here is
// non-fatal — log at most — because on some setups (read-only bind mounts,
// volume mounts) chmod simply won't work, and we don't want to crash the
// server over a permission hint.
func secureDB(path string) {
	files := []string{path, path + "-wal", path + "-shm", path + "-journal"}
	for _, f := range files {
		if _, err := os.Stat(f); err == nil {
			_ = os.Chmod(f, 0600)
		}
	}
	if dir := filepath.Dir(path); dir != "." && dir != "/" {
		_ = os.Chmod(dir, 0700)
	}
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
    next_run_at  DATETIME NOT NULL,
    enabled      INTEGER NOT NULL DEFAULT 1,
    parent_id    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_tasks_status_next ON tasks(status, next_run_at);
CREATE INDEX IF NOT EXISTS idx_tasks_type       ON tasks(type);
CREATE INDEX IF NOT EXISTS idx_tasks_parent_id  ON tasks(parent_id);
`
	if _, err := s.db.Exec(ddl); err != nil {
		return err
	}
	// Old databases predate the enabled column. Check via pragma_table_info
	// before ALTER so we never surface a "duplicate column" error that could
	// leave the single connection pool in a bad state.
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name='enabled'`).Scan(&n); err != nil {
		return fmt.Errorf("check enabled column: %w", err)
	}
	if n == 0 {
		if _, err := s.db.Exec(`ALTER TABLE tasks ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1`); err != nil {
			return fmt.Errorf("add enabled column: %w", err)
		}
	}
	// parent_id column (cron child linkage) — same migration pattern.
	var p int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name='parent_id'`).Scan(&p); err != nil {
		return fmt.Errorf("check parent_id column: %w", err)
	}
	if p == 0 {
		if _, err := s.db.Exec(`ALTER TABLE tasks ADD COLUMN parent_id INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add parent_id column: %w", err)
		}
	}
	return nil
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
	// New tasks are enabled by default; pausing only happens via SetEnabled.
	t.Enabled = true
	res, err := s.db.ExecContext(ctx, `
INSERT INTO tasks(name, command, type, cron_expr, status, max_retry, retry_count, timeout_sec, worker_id, parent_id, result, created_at, updated_at, next_run_at, enabled)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.Name, t.Command, t.Type, t.CronExpr, t.Status, t.MaxRetry, t.RetryCount,
		t.TimeoutSec, t.WorkerID, t.ParentID, t.Result, t.CreatedAt, t.UpdatedAt, t.NextRunAt, t.Enabled)
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	t.ID, _ = res.LastInsertId()
	return nil
}

// Delete removes a task row. Idempotent: deleting a non-existent id is a no-op.
// This is the only way to get rid of a completed one-shot task or to fully
// remove a cron definition.
func (s *Store) Delete(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tasks WHERE id=?`, id)
	return err
}

// SetEnabled toggles a task's enabled flag. For cron tasks, the scheduler
// skips disabled definitions on its next resync (within syncEvery). The
// flag is informational for one-shot tasks — they have already run or will
// run once regardless — but we still honour it for consistency.
//
// When disabling a cron definition (enabled=false), this operation also
// marks every in-flight child task (parent_id=id, status pending|running)
// as stopped within the same transaction, so workers cancel them via the
// stop list and no stale children execute after the parent is paused.
func (s *Store) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	v := 0
	if enabled {
		v = 1
	}
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET enabled=?, updated_at=? WHERE id=?`, v, time.Now(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}

	// Pausing a cron def: stop all pending/running children atomically so
	// workers abandon them. Already-finished children are left untouched.
	if !enabled {
		_, err = tx.ExecContext(ctx, `
UPDATE tasks SET status=?, result='stopped: cron paused', updated_at=?
WHERE parent_id=? AND status IN (?,?)`,
			model.StatusStopped, time.Now(), id, model.StatusPending, model.StatusRunning)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// Get fetches a single task by id. Returns ErrNotFound when absent.
func (s *Store) Get(ctx context.Context, id int64) (*model.Task, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, name, command, type, cron_expr, status, max_retry, retry_count, timeout_sec, worker_id, parent_id, result, created_at, updated_at, next_run_at, enabled
FROM tasks WHERE id=?`, id)
	t := &model.Task{}
	err := row.Scan(&t.ID, &t.Name, &t.Command, &t.Type, &t.CronExpr, &t.Status,
		&t.MaxRetry, &t.RetryCount, &t.TimeoutSec, &t.WorkerID, &t.ParentID, &t.Result,
		&t.CreatedAt, &t.UpdatedAt, &t.NextRunAt, &t.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ListFilter describes the optional constraints applied by List. All fields
// are optional; empty/zero values mean "no filter on this column".
type ListFilter struct {
	Name     string // substring match on name
	Command  string // substring match on command
	Type     string // exact match on type
	Status   string // comma-separated statuses, or "done" alias
	Worker   string // substring match on worker_id
	Enabled  string // "", "true", "false"
	ParentID int64  // 0 = top-level tasks only (excludes cron children); >0 = children of that cron
}

// splitStatuses splits a comma-separated status filter into a non-empty
// slice, dropping blanks. Returns nil when nothing to filter on.
func splitStatuses(filter string) []string {
	if filter == "" {
		return nil
	}
	parts := strings.Split(filter, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// buildWhere turns a ListFilter into a parameterised WHERE clause. Returns
// the clause (without the WHERE keyword) and the args slice. Shared by List
// and Count so the two queries always agree on which rows match.
func buildWhere(f *ListFilter) (string, []any) {
	if f == nil {
		f = &ListFilter{}
	}
	// "done" is a convenience alias that matches all terminal states.
	statuses := splitStatuses(f.Status)
	if f.Status == "done" {
		statuses = []string{
			string(model.StatusSuccess),
			string(model.StatusFailed),
			string(model.StatusDead),
		}
	}
	var (
		conds []string
		args  []any
	)
	if f.Name != "" {
		conds = append(conds, "name LIKE ?")
		args = append(args, "%"+f.Name+"%")
	}
	if f.Command != "" {
		conds = append(conds, "command LIKE ?")
		args = append(args, "%"+f.Command+"%")
	}
	if f.Type != "" {
		conds = append(conds, "type = ?")
		args = append(args, f.Type)
	}
	if len(statuses) > 0 {
		placeholders := make([]string, len(statuses))
		for i, st := range statuses {
			placeholders[i] = "?"
			args = append(args, st)
		}
		conds = append(conds, "status IN ("+strings.Join(placeholders, ",")+")")
	}
	if f.Worker != "" {
		conds = append(conds, "worker_id LIKE ?")
		args = append(args, "%"+f.Worker+"%")
	}
	if f.Enabled == "true" {
		conds = append(conds, "enabled = 1")
	} else if f.Enabled == "false" {
		conds = append(conds, "enabled = 0")
	}
	// parent_id drives the two views:
	//   - default (ParentID==0): show only top-level tasks, hiding cron
	//     child instances from the all-tasks list.
	//   - ParentID>0: show only the children spawned by that cron def.
	if f.ParentID > 0 {
		conds = append(conds, "parent_id = ?")
		args = append(args, f.ParentID)
	} else {
		conds = append(conds, "parent_id = 0")
	}
	if len(conds) == 0 {
		return "", args
	}
	return strings.Join(conds, " AND "), args
}

// List returns tasks matching f, newest first, starting at offset, capped at
// limit. f is a pointer so callers can pass &ListFilter{} for "no constraints".
func (s *Store) List(ctx context.Context, f *ListFilter, offset, limit int) ([]*model.Task, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	where, args := buildWhere(f)
	q := `SELECT id, name, command, type, cron_expr, status, max_retry, retry_count, timeout_sec, worker_id, parent_id, result, created_at, updated_at, next_run_at, enabled
FROM tasks`
	if where != "" {
		q += " WHERE " + where
	}
	q += " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Task
	for rows.Next() {
		t := &model.Task{}
		if err := rows.Scan(&t.ID, &t.Name, &t.Command, &t.Type, &t.CronExpr,
			&t.Status, &t.MaxRetry, &t.RetryCount, &t.TimeoutSec, &t.WorkerID,
			&t.ParentID, &t.Result, &t.CreatedAt, &t.UpdatedAt, &t.NextRunAt, &t.Enabled); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Count returns the total number of tasks matching f. Used by the paginated
// list view to compute page numbers.
func (s *Store) Count(ctx context.Context, f *ListFilter) (int64, error) {
	where, args := buildWhere(f)
	q := "SELECT COUNT(*) FROM tasks"
	if where != "" {
		q += " WHERE " + where
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ListCron returns the enabled cron definition rows the scheduler iterates
// over. Paused (enabled=0) definitions are excluded so they stop spawning
// child tasks on the next resync.
func (s *Store) ListCron(ctx context.Context) ([]*model.Task, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, command, type, cron_expr, status, max_retry, retry_count, timeout_sec, worker_id, parent_id, result, created_at, updated_at, next_run_at, enabled
FROM tasks WHERE type=? AND enabled=1`, model.ScheduleCron)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Task
	for rows.Next() {
		t := &model.Task{}
		if err := rows.Scan(&t.ID, &t.Name, &t.Command, &t.Type, &t.CronExpr,
			&t.Status, &t.MaxRetry, &t.RetryCount, &t.TimeoutSec, &t.WorkerID,
			&t.ParentID, &t.Result, &t.CreatedAt, &t.UpdatedAt, &t.NextRunAt, &t.Enabled); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// StopAllCron performs a bulk shutdown of cron activity:
//  1. disables every cron definition (enabled=0) so no new children spawn,
//  2. marks every in-flight cron child (parent_id>0, status pending|running)
//     as stopped so workers cancel them via the stop list.
//
// Returns the counts of paused definitions and stopped children.
func (s *Store) StopAllCron(ctx context.Context) (pausedDefs, stoppedChildren int64, err error) {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET enabled=0, updated_at=? WHERE type=?`,
		time.Now(), model.ScheduleCron)
	if err != nil {
		return 0, 0, err
	}
	pausedDefs, _ = res.RowsAffected()

	res, err = s.db.ExecContext(ctx, `
UPDATE tasks SET status=?, result='stopped: cron shutdown', updated_at=?
WHERE parent_id>0 AND status IN (?,?)`,
		model.StatusStopped, time.Now(), model.StatusPending, model.StatusRunning)
	if err != nil {
		return pausedDefs, 0, err
	}
	stoppedChildren, _ = res.RowsAffected()
	return pausedDefs, stoppedChildren, nil
}

// StopTask marks a task stopped. For pending tasks this is a no-op state
// change; for running tasks the row is flipped to stopped and the owning
// worker will discover the stop via the StopList returned by the next pull
// and cancel the in-flight process. Idempotent on already-stopped rows.
func (s *Store) StopTask(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE tasks SET status=?, result=?, updated_at=? WHERE id=? AND status IN (?,?)`,
		model.StatusStopped, "stopped by user", time.Now(), id,
		model.StatusPending, model.StatusRunning)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ResumeTask returns a stopped task to pending so it can be claimed again.
// Only stopped tasks are eligible; other states are left untouched.
func (s *Store) ResumeTask(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE tasks SET status=?, worker_id='', result='', updated_at=?, next_run_at=?
WHERE id=? AND status=?`,
		model.StatusPending, time.Now(), time.Now(), id, model.StatusStopped)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetStopList returns the ids of tasks owned by workerID that are currently
// marked stopped. The worker uses this to cancel in-flight processes it
// started before the stop was issued.
func (s *Store) GetStopList(ctx context.Context, workerID string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id FROM tasks WHERE worker_id=? AND status=?`, workerID, model.StatusStopped)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ClearWorker clears the worker_id on stopped tasks after the worker has
// acknowledged the stop. Without this, the stop list would keep returning
// the same ids until the row is resumed or deleted.
func (s *Store) ClearWorker(ctx context.Context, taskID int64, workerID string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE tasks SET worker_id='' WHERE id=? AND worker_id=? AND status=?`,
		taskID, workerID, model.StatusStopped)
	return err
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
SELECT id, name, command, type, cron_expr, status, max_retry, retry_count, timeout_sec, worker_id, parent_id, result, created_at, updated_at, next_run_at, enabled
FROM tasks
WHERE type=? AND status=? AND next_run_at<=?
ORDER BY next_run_at ASC
LIMIT 1`, model.ScheduleOnce, model.StatusPending, time.Now())
	t := &model.Task{}
	err = row.Scan(&t.ID, &t.Name, &t.Command, &t.Type, &t.CronExpr, &t.Status,
		&t.MaxRetry, &t.RetryCount, &t.TimeoutSec, &t.WorkerID, &t.ParentID, &t.Result,
		&t.CreatedAt, &t.UpdatedAt, &t.NextRunAt, &t.Enabled)
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

	// If the task was stopped while the worker was executing it, keep the
	// stopped state — the user's intent takes precedence over the outcome.
	// Just detach the worker so the stop list stops nagging.
	if status == string(model.StatusStopped) {
		_, err = tx.ExecContext(ctx, `UPDATE tasks SET worker_id='' WHERE id=? AND status=?`,
			r.TaskID, model.StatusStopped)
		if err != nil {
			return err
		}
		return tx.Commit()
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
