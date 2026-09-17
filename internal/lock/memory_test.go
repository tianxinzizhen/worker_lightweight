package lock

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestAcquireRelease verifies the happy path: acquire then release lets a
// second acquire succeed.
func TestAcquireRelease(t *testing.T) {
	l := NewMemory()
	if err := l.Acquire("k", "tok1", time.Second); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := l.Acquire("k", "tok2", time.Second); err != ErrAlreadyHeld {
		t.Fatalf("second acquire expected ErrAlreadyHeld, got %v", err)
	}
	if err := l.Release("k", "tok1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := l.Acquire("k", "tok2", time.Second); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

// TestReleaseWrongToken ensures Release with a stale token does NOT free the
// lock — preventing a slow holder from releasing a lease already re-acquired
// by another owner after expiry.
func TestReleaseWrongToken(t *testing.T) {
	l := NewMemory()
	_ = l.Acquire("k", "tok1", time.Second)
	// Releasing with the wrong token is a no-op; the lock stays held.
	if err := l.Release("k", "wrong"); err != nil {
		t.Fatalf("release wrong token should be nil, got %v", err)
	}
	if err := l.Acquire("k", "tok2", time.Second); err != ErrAlreadyHeld {
		t.Fatalf("lock should still be held, got %v", err)
	}
}

// TestExpiry verifies the lock auto-releases after the ttl elapses so a
// crashed holder can't pin a cron task forever.
func TestExpiry(t *testing.T) {
	l := NewMemory()
	_ = l.Acquire("k", "tok1", 30*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	if err := l.Acquire("k", "tok2", time.Second); err != nil {
		t.Fatalf("acquire after expiry should succeed, got %v", err)
	}
}

// TestConcurrentAcquire hammers the lock from many goroutines, each with a
// distinct token (mimicking distinct workers). Exactly one must win — the
// rest get ErrAlreadyHeld. This is the property the cron scheduler depends on
// to avoid duplicate task spawns.
func TestConcurrentAcquire(t *testing.T) {
	l := NewMemory()
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	holders := 0
	const n = 100
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := l.Acquire("k", fmt.Sprintf("tok-%d", i), 5*time.Second)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				winners++
			} else if err == ErrAlreadyHeld {
				holders++
			}
		}(i)
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", winners)
	}
	if holders != n-1 {
		t.Fatalf("expected %d holders, got %d", n-1, holders)
	}
}
