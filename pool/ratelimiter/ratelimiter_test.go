package ratelimiter

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRateLimiter_BasicAcquireRelease(t *testing.T) {
	rl := NewRateLimiter(1 * time.Second)

	// First acquire should succeed
	if !rl.TryAcquire() {
		t.Error("expected first TryAcquire to succeed")
	}

	// Release and wait for cooldown
	rl.Release()
	time.Sleep(2 * time.Second)

	// Should be available again
	if !rl.TryAcquire() {
		t.Error("expected TryAcquire to succeed after cooldown")
	}
}

func TestRateLimiter_CooldownRestoresAvailability(t *testing.T) {
	cooldown := 1 * time.Second
	rl := NewRateLimiter(cooldown)

	// Acquire the token
	if !rl.TryAcquire() {
		t.Fatal("initial TryAcquire should succeed")
	}

	// Release it
	rl.Release()

	// Should not be available immediately
	if rl.TryAcquire() {
		t.Error("token should not be available immediately after release")
	}

	// Wait for cooldown to complete
	time.Sleep(cooldown + 1*time.Second)

	// Now it should be available
	if !rl.TryAcquire() {
		t.Error("token should be available after cooldown period")
	}
}

func TestRateLimiter_ZeroCooldown(t *testing.T) {
	rl := NewRateLimiter(0)

	// Acquire the token
	if !rl.TryAcquire() {
		t.Fatal("initial TryAcquire should succeed")
	}

	// Release with zero cooldown
	rl.Release()

	// With zero cooldown, AfterFunc fires immediately (or very soon)
	time.Sleep(1 * time.Second)

	// Should be available again almost immediately
	if !rl.TryAcquire() {
		t.Error("token should be available after zero cooldown")
	}
}

func TestRateLimiter_MultipleAcquireWhenUnavailable(t *testing.T) {
	rl := NewRateLimiter(5 * time.Second)

	// Acquire the token
	if !rl.TryAcquire() {
		t.Fatal("initial TryAcquire should succeed")
	}

	// Multiple attempts should all fail
	for i := range 10 {
		if rl.TryAcquire() {
			t.Errorf("TryAcquire attempt %d should have failed", i)
		}
	}
}

func TestRateLimiter_RaceCondition(t *testing.T) {
	rl := NewRateLimiter(1 * time.Second)

	const numGoroutines = 100
	const iterations = 10

	var successCount atomic.Int64
	var wg sync.WaitGroup

	wg.Add(numGoroutines)
	for range numGoroutines {
		go func() {
			defer wg.Done()
			for range iterations {
				if rl.TryAcquire() {
					successCount.Add(1)
					rl.Release()
				}
			}
		}()
	}

	wg.Wait()

	if successCount.Load() == 0 {
		t.Error("expected at least one successful acquires")
	}

	time.Sleep(2 * time.Second)

	// After all goroutines finish and cooldowns complete, token should be available
	rl.mu.Lock()
	if !rl.available {
		t.Error("rate limiter should be in available state after all operations complete")
	}
	rl.mu.Unlock()
}
