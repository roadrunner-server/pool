package ratelimiter

import (
	"sync/atomic"
	"time"
)

type RateLimiter struct {
	available atomic.Bool
	cooldown  time.Duration
}

func NewRateLimiter(cooldown time.Duration) *RateLimiter {
	rl := &RateLimiter{
		cooldown: cooldown,
	}
	rl.available.Store(true)

	return rl
}

// TryAcquire attempts to take the token. Returns false immediately if unavailable.
func (rl *RateLimiter) TryAcquire() bool {
	return rl.available.CompareAndSwap(true, false)
}

// Release returns the token after the cooldown period.
func (rl *RateLimiter) Release() {
	time.AfterFunc(rl.cooldown, func() {
		rl.available.Store(true)
	})
}
