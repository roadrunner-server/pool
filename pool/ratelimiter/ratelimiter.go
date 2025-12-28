package ratelimiter

import (
	"sync"
	"time"
)

type RateLimiter struct {
	mu        sync.Mutex
	available bool
	cooldown  time.Duration
}

func NewRateLimiter(cooldown time.Duration) *RateLimiter {
	return &RateLimiter{
		available: true,
		cooldown:  cooldown,
	}
}

// TryAcquire attempts to take the token. Returns false immediately if unavailable.
func (rl *RateLimiter) TryAcquire() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if rl.available {
		rl.available = false
		return true
	}
	return false
}

// Release returns the token after the cooldown period.
func (rl *RateLimiter) Release() {
	time.AfterFunc(rl.cooldown, func() {
		rl.mu.Lock()
		rl.available = true
		rl.mu.Unlock()
	})
}
