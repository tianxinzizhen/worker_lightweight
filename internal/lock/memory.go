// Package lock provides a pluggable distributed-lock abstraction. The default
// in-memory implementation guards against concurrent duplicate cron-tick
// spawns within a single server process. The interface is shaped so a Redis
// implementation (SET NX EX) could be dropped in without touching callers.
package lock

import (
	"errors"
	"sync"
	"time"
)

// ErrAlreadyHeld means the lock is currently owned by someone. Callers must
// NOT treat this as a hard error — for cron dedup it is the normal path.
var ErrAlreadyHeld = errors.New("lock already held")

// Lock is the distributed-lock contract used by the scheduler.
type Lock interface {
	// Acquire tries to grab the lock named key for ttl. Returns
	// ErrAlreadyHeld when another owner holds it. Callers must release
	// the returned token via Release, even on best-effort cleanup.
	Acquire(key, token string, ttl time.Duration) error
	// Release frees the lock only if the caller presents the same token
	// that Acquire returned — preventing a slow worker from releasing a
	// lock that has already expired and been re-acquired by someone else.
	Release(key, token string) error
}

// NewMemory returns a process-local Lock backed by a sync.Map. Sufficient
// for a single server; swap in a RedisLock for multi-server HA.
func NewMemory() Lock { return &memoryLock{m: sync.Map{}} }

type entry struct {
	token string
	exp   time.Time
}

type memoryLock struct{ m sync.Map }

func (l *memoryLock) Acquire(key, token string, ttl time.Duration) error {
	now := time.Now()
	v, loaded := l.m.LoadOrStore(key, &entry{token: token, exp: now.Add(ttl)})
	e := v.(*entry)
	if !loaded {
		return nil
	}
	// Already present: only win if the previous lease expired.
	if now.After(e.exp) {
		// Try CAS-style swap. On failure someone beat us.
		if e.exp.Before(now) {
			e.token = token
			e.exp = now.Add(ttl)
			return nil
		}
	}
	if e.token == token {
		// Re-entrant refresh by the same owner.
		e.exp = now.Add(ttl)
		return nil
	}
	return ErrAlreadyHeld
}

func (l *memoryLock) Release(key, token string) error {
	v, ok := l.m.Load(key)
	if !ok {
		return nil
	}
	e := v.(*entry)
	if e.token != token {
		// Lock was re-acquired by another owner; do nothing.
		return nil
	}
	l.m.Delete(key)
	return nil
}
