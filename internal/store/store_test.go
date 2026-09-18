package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tianxinzizhen/worker_lightweight/internal/model"
)

// newTestStore returns a fresh store backed by a temp file. Using a real
// SQLite file (not :memory:) keeps WAL behaviour representative.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustCreate(t *testing.T, s *Store, name string) *model.Task {
	t.Helper()
	task := &model.Task{
		Name:    name,
		Command: "echo hi",
		Type:    model.ScheduleOnce,
		Status:  model.StatusPending,
	}
	if err := s.Create(context.Background(), task); err != nil {
		t.Fatalf("create: %v", err)
	}
	return task
}

func TestCreateAndGet(t *testing.T) {
	s := newTestStore(t)
	task := mustCreate(t, s, "t1")
	got, err := s.Get(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "t1" || got.Status != model.StatusPending {
		t.Fatalf("unexpected task: %+v", got)
	}
}

func TestGetNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Get(context.Background(), 9999); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestClaimPendingConcurrent is the core anti-duplicate guarantee: 50 workers
// race to claim a single pending task; exactly one must win. The atomic
// UPDATE ... WHERE status='pending' is what enforces this.
func TestClaimPendingConcurrent(t *testing.T) {
	s := newTestStore(t)
	mustCreate(t, s, "race")

	var wg sync.WaitGroup
	mu := &sync.Mutex{}
	claimed := 0
	notFound := 0
	const n = 50
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.ClaimPending(context.Background(), fmt.Sprintf("w%d", i))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				claimed++
			} else if err == ErrNotFound {
				notFound++
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if claimed != 1 {
		t.Fatalf("expected exactly 1 claim, got %d", claimed)
	}
	if notFound != n-1 {
		t.Fatalf("expected %d not-found, got %d", n-1, notFound)
	}
}

// TestReportResultRetry verifies the retry path: a failed report with
// retries remaining flips the task back to pending and bumps retry_count.
func TestReportResultRetry(t *testing.T) {
	s := newTestStore(t)
	task := &model.Task{
		Name:     "retry-test",
		Command:  "false",
		Type:     model.ScheduleOnce,
		Status:   model.StatusPending,
		MaxRetry: 2,
	}
	if err := s.Create(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	// Claim it first so the report owner check passes.
	claimed, err := s.ClaimPending(context.Background(), "w1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Report failure; with MaxRetry=2 it should go back to pending.
	if err := s.ReportResult(context.Background(), &model.Result{
		TaskID: claimed.ID, WorkerID: "w1", Success: false, Output: "boom",
	}); err != nil {
		t.Fatalf("report: %v", err)
	}
	got, _ := s.Get(context.Background(), claimed.ID)
	if got.Status != model.StatusPending {
		t.Fatalf("expected pending after retry, got %s", got.Status)
	}
	if got.RetryCount != 1 {
		t.Fatalf("expected retry_count=1, got %d", got.RetryCount)
	}
	if got.WorkerID != "" {
		t.Fatalf("expected worker cleared on retry, got %q", got.WorkerID)
	}
}

// TestReportResultDead verifies that exceeding MaxRetry marks the task dead.
func TestReportResultDead(t *testing.T) {
	s := newTestStore(t)
	task := &model.Task{
		Name: "dead-test", Command: "false", Type: model.ScheduleOnce,
		Status: model.StatusPending, MaxRetry: 0, // no retries allowed
	}
	if err := s.Create(context.Background(), task); err != nil {
		t.Fatalf("create: %v", err)
	}
	claimed, err := s.ClaimPending(context.Background(), "w1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil {
		t.Fatal("claim returned nil task")
	}
	if err := s.ReportResult(context.Background(), &model.Result{
		TaskID: claimed.ID, WorkerID: "w1", Success: false, Output: "boom",
	}); err != nil {
		t.Fatalf("report: %v", err)
	}
	got, _ := s.Get(context.Background(), claimed.ID)
	if got.Status != model.StatusDead {
		t.Fatalf("expected dead, got %s", got.Status)
	}
}

// TestReportResultSuccess verifies the success path marks status success.
func TestReportResultSuccess(t *testing.T) {
	s := newTestStore(t)
	mustCreate(t, s, "ok-test")
	claimed, _ := s.ClaimPending(context.Background(), "w1")
	if err := s.ReportResult(context.Background(), &model.Result{
		TaskID: claimed.ID, WorkerID: "w1", Success: true, Output: "done",
	}); err != nil {
		t.Fatalf("report: %v", err)
	}
	got, _ := s.Get(context.Background(), claimed.ID)
	if got.Status != model.StatusSuccess {
		t.Fatalf("expected success, got %s", got.Status)
	}
}

// TestClaimPendingRespectsNextRunAt verifies that a task with a future
// next_run_at is NOT claimable until that time. This underpins scheduled
// one-shot tasks.
func TestClaimPendingRespectsNextRunAt(t *testing.T) {
	s := newTestStore(t)
	task := &model.Task{
		Name:      "future",
		Command:   "echo",
		Type:      model.ScheduleOnce,
		Status:    model.StatusPending,
		NextRunAt: time.Now().Add(time.Hour),
	}
	_ = s.Create(context.Background(), task)
	if _, err := s.ClaimPending(context.Background(), "w1"); err != ErrNotFound {
		t.Fatalf("future task should not be claimable, got %v", err)
	}
}

// TestReportResultWrongOwner verifies the ownership guard: only the worker
// that claimed the task may report its result.
func TestReportResultWrongOwner(t *testing.T) {
	s := newTestStore(t)
	mustCreate(t, s, "owner-test")
	claimed, _ := s.ClaimPending(context.Background(), "w1")
	err := s.ReportResult(context.Background(), &model.Result{
		TaskID: claimed.ID, WorkerID: "impostor", Success: true,
	})
	if err == nil {
		t.Fatal("expected error from wrong owner, got nil")
	}
}
