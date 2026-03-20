// Dynamic allocator for the static pool implementation
// It allocates new workers with batch spawn rate when there are no free workers
// It uses 2 functions: addMoreWorkers to allocate new workers and startIdleTTLListener
package static_pool

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"log/slog"

	"github.com/roadrunner-server/pool/v2/pool"
	"github.com/roadrunner-server/pool/v2/pool/ratelimiter"
	"github.com/roadrunner-server/pool/v2/worker"
	"github.com/roadrunner-server/pool/v2/worker_watcher"
)

type dynAllocator struct {
	// derived from the config
	maxWorkers  uint64
	spawnRate   uint64
	idleTimeout time.Duration

	// internal
	currAllocated atomic.Uint64
	mu            *sync.Mutex
	started       atomic.Bool
	log           *slog.Logger
	// pool
	ww        *worker_watcher.WorkerWatcher
	allocator func() (*worker.Process, error)
	stopCh    chan struct{}
	// the case is, that multiple goroutines can call addMoreWorkers at the same time
	// and we need to omit some NoFreeWorker calls if one is already in progress within the same time frame
	rateLimit    *ratelimiter.RateLimiter
	lastAllocTry atomic.Pointer[time.Time]
}

func newDynAllocator(
	log *slog.Logger,
	ww *worker_watcher.WorkerWatcher,
	alloc func() (*worker.Process, error),
	stopCh chan struct{},
	cfg *pool.Config) *dynAllocator {
	da := &dynAllocator{
		maxWorkers:  cfg.DynamicAllocatorOpts.MaxWorkers,
		spawnRate:   cfg.DynamicAllocatorOpts.SpawnRate,
		idleTimeout: cfg.DynamicAllocatorOpts.IdleTimeout,
		mu:          &sync.Mutex{},
		ww:          ww,
		allocator:   alloc,
		log:         log,
		stopCh:      stopCh,
		rateLimit:   ratelimiter.NewRateLimiter(time.Second),
	}

	da.currAllocated.Store(0)
	da.started.Store(false)

	return da
}

func (da *dynAllocator) addMoreWorkers() {
	// set the last allocation try time
	// we need to store this to prevent immediate deallocation in the TTL listener
	da.lastAllocTry.Store(new(time.Now().UTC()))

	if !da.rateLimit.TryAcquire() {
		da.log.Warn("rate limit exceeded for dynamic allocation, skipping")
		return
	}

	// return the token after 1 second
	defer da.rateLimit.Release()

	// operation lock
	da.mu.Lock()
	defer da.mu.Unlock()

	da.log.Debug("No free workers, trying to allocate dynamically",
		"idle_timeout", da.idleTimeout,
		"max_workers", da.maxWorkers,
		"spawn_rate", da.spawnRate)

	if !da.started.Load() {
		// start the dynamic allocator listener
		da.startIdleTTLListener()
		da.started.Store(true)
	}

	// if we already allocated max workers, we can't allocate more
	if da.currAllocated.Load() >= da.maxWorkers {
		// can't allocate more
		da.log.Warn("can't allocate more workers, already allocated max workers", "max_workers", da.maxWorkers)
		return
	}

	// we're starting from the 1 because we already allocated one worker which would be released in the Exec function
	// i < da.spawnRate - we can't allocate more workers than the spawn rate
	for range da.spawnRate {
		// spawn as many workers as the user specified in the spawn rate configuration, but not more than max workers
		if da.currAllocated.Load() >= da.maxWorkers {
			break
		}

		err := da.ww.AddWorker()
		if err != nil {
			da.log.Error("failed to allocate worker", "error", err)
			continue
		}

		// increase the number of additionally allocated options
		aw := da.currAllocated.Add(1)
		da.log.Debug("allocated additional worker", "currently additionally allocated", aw)
	}

	da.log.Debug("currently allocated", "number", da.currAllocated.Load())
}

func (da *dynAllocator) startIdleTTLListener() {
	da.log.Debug("starting dynamic allocator listener", "idle_timeout", da.idleTimeout)
	go func() {
		// DynamicAllocatorOpts are read-only, so we can use them without a lock
		triggerTTL := time.NewTicker(da.idleTimeout)
		defer triggerTTL.Stop()

		for {
			select {
			case <-da.stopCh:
				da.log.Debug("dynamic allocator listener stopped")
				// Acquire lock before setting started=false to prevent race with addMoreWorkers
				da.mu.Lock()
				da.started.Store(false)
				da.mu.Unlock()
				da.log.Debug("dynamic allocator listener exited")
				return
			// when this channel is triggered, we should deallocate all dynamically allocated workers
			case <-triggerTTL.C:
				da.log.Debug("dynamic workers TTL", "reason", "idle timeout reached")
				// check the last allocation time - if we had an allocation recently (within idleTimeout), we should skip deallocation
				lastAlloc := da.lastAllocTry.Load()
				if lastAlloc != nil && time.Since(*lastAlloc) < da.idleTimeout {
					da.log.Debug("skipping deallocation of dynamic workers, recent allocation detected")
					continue
				}

				// get the DynamicAllocatorOpts lock to prevent operations on the CurrAllocated
				da.mu.Lock()

				// if we don't have any dynamically allocated workers, we can skip the deallocation
				if da.currAllocated.Load() == 0 {
					// Set started=false BEFORE releasing the lock
					// This prevents the race condition where addMoreWorkers() sees started=true
					// but the listener is about to exit
					da.started.Store(false)
					da.mu.Unlock()
					da.log.Debug("dynamic allocator listener exited, no workers to deallocate")
					return
				}

				alloc := da.currAllocated.Load()
				da.log.Debug("deallocating dynamically allocated workers", "to_deallocate", alloc)

				if alloc >= da.spawnRate {
					// deallocate in batches
					alloc = da.spawnRate
				}

				for range alloc {
					// Use a context with timeout to prevent indefinite blocking
					// The timeout should be reasonable - use idle timeout as a reference
					ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*500)
					err := da.ww.RemoveWorker(ctx)
					cancel()
					// the only error we can get here is NoFreeWorkers, meaning all workers are busy
					if err != nil {
						// we should stop deallocation attempts
						da.log.Error("failed to remove worker from the pool, stopping deallocation", "error", err)
						// Don't decrement counter if removal failed - worker still exists
						break
					}

					// decrease the number of additionally allocated workers
					nw := da.currAllocated.Add(^uint64(0))
					da.log.Debug("deallocated additional worker", "currently additionally allocated", nw)
				}

				if da.currAllocated.Load() > 0 {
					// if we still have allocated workers, we should keep the listener running
					da.mu.Unlock()
					da.log.Debug("dynamic allocator listener continuing, still have dynamically allocated workers", "remaining", da.currAllocated.Load())
					continue
				}

				// CRITICAL FIX: Set started=false BEFORE releasing the lock
				// This ensures that any addMoreWorkers() call that acquires the lock
				// after this point will see started=false and start a new listener
				da.started.Store(false)
				da.lastAllocTry.Store(nil)
				da.mu.Unlock()
				da.log.Debug("dynamic allocator listener exited, all dynamically allocated workers deallocated")
				return
			}
		}
	}()
}
