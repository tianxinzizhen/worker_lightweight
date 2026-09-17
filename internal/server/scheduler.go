// scheduler.go drives recurring (cron) tasks. On each tick it asks the
// cron library which definitions are due, then — under a per-task lock —
// enqueues a fresh one-shot child task for workers to claim. The lock
// guarantees a single spawn even if two ticks fire back-to-back.
package server

import (
	"context"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/tianxinzizhen/worker_lightweight/internal/lock"
	"github.com/tianxinzizhen/worker_lightweight/internal/logger"
	"github.com/tianxinzizhen/worker_lightweight/internal/model"
	"github.com/tianxinzizhen/worker_lightweight/internal/store"
)

// Scheduler owns one cron.Cron instance whose schedule is rebuilt whenever
// the set of cron definitions changes. We rebuild on a slow polling cadence
// to keep the design simple; for production a notification on insert would
// be better, but polling keeps the surface area small.
type Scheduler struct {
	store     *store.Store
	lock      lock.Lock
	cron      *cron.Cron
	syncEvery time.Duration
}

// NewScheduler returns a scheduler ready to Start. syncEvery controls how
// often the scheduler refreshes its cron entry table from the database.
func NewScheduler(s *store.Store, l lock.Lock, syncEvery time.Duration) *Scheduler {
	c := cron.New(cron.WithSeconds()) // 6-field expressions including seconds
	return &Scheduler{store: s, lock: l, cron: c, syncEvery: syncEvery}
}

// Start launches the cron engine and the sync loop. Cancel ctx to stop.
func (s *Scheduler) Start(ctx context.Context) {
	s.cron.Start()
	ticker := time.NewTicker(s.syncEvery)
	defer ticker.Stop()
	// Prime immediately so we don't wait a full interval at boot.
	s.resync(ctx)
	for {
		select {
		case <-ctx.Done():
			logger.L.Info("scheduler stopping")
			stopCtx := s.cron.Stop()
			<-stopCtx.Done()
			return
		case <-ticker.C:
			s.resync(ctx)
		}
	}
}

// resync rebuilds the cron entry set from the database. It is idempotent:
// the whole schedule is torn down and re-added, which is cheap because the
// definition count is small.
func (s *Scheduler) resync(ctx context.Context) {
	defs, err := s.store.ListCron(ctx)
	if err != nil {
		logger.L.Error("scheduler: list cron defs failed", "err", err)
		return
	}
	// Replace entries: stop all current and add fresh ones.
	for _, e := range s.cron.Entries() {
		s.cron.Remove(e.ID)
	}
	for _, d := range defs {
		d := d
		if _, err := s.cron.AddFunc(d.CronExpr, func() { s.spawn(ctx, d) }); err != nil {
			logger.L.Error("scheduler: invalid cron expression", "expr", d.CronExpr, "err", err)
		}
	}
	logger.L.Debug("scheduler resync complete", "cron_count", len(defs))
}

// spawn creates a one-shot child task derived from the cron definition. The
// per-task lock prevents duplicate spawns when ticks overlap (e.g. a slow
// previous tick still holding the lock at the next fire time).
func (s *Scheduler) spawn(ctx context.Context, d *model.Task) {
	token := time.Now().Format("20060102T150405.000")
	key := "cron:" + d.Name
	if err := s.lock.Acquire(key, token, 30*time.Second); err != nil {
		logger.L.Debug("scheduler: spawn skipped, lock held", "task", d.Name)
		return
	}
	defer func() { _ = s.lock.Release(key, token) }()

	child := &model.Task{
		Name:       d.Name + "#" + token,
		Command:    d.Command,
		Type:       model.ScheduleOnce,
		Status:     model.StatusPending,
		MaxRetry:   d.MaxRetry,
		TimeoutSec: d.TimeoutSec,
	}
	if err := s.store.Create(ctx, child); err != nil {
		logger.L.Error("scheduler: spawn child failed", "task", d.Name, "err", err)
		return
	}
	logger.L.Info("scheduler: spawned child task", "parent", d.Name, "child_id", child.ID)
}
