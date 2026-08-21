// Dynamic allocator for the static pool. When a request finds no free worker within the
// allocate timeout, addMoreWorkers spawns a batch of spawnRate workers (rate limited to one
// batch per cooldown) up to maxWorkers above the base pool size. An idle-TTL listener removes
// the extra workers in spawnRate-sized batches once no allocation pressure was seen for
// idleTimeout.
package static_pool

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"log/slog"

	"github.com/roadrunner-server/pool/v2/pool"
	"github.com/roadrunner-server/pool/v2/pool/ratelimiter"
	"github.com/roadrunner-server/pool/v2/worker_watcher"
)

type dynAllocator struct {
	// derived from the config
	maxWorkers  uint64
	spawnRate   uint64
	idleTimeout time.Duration
	// base pool size; everything above it is managed by this allocator
	baseWorkers uint64

	mu sync.Mutex
	// idle-TTL listener lifecycle flag, guarded by mu
	started bool
	log     *slog.Logger
	ww      *worker_watcher.WorkerWatcher
	stopCh  chan struct{}
	// collapses concurrent NoFreeWorkers triggers into one spawn batch per cooldown
	rateLimit *ratelimiter.RateLimiter
	// unix nano of the last allocation trigger; postpones idle deallocation while
	// allocation pressure is present
	lastAllocTry atomic.Int64
}

func newDynAllocator(log *slog.Logger, ww *worker_watcher.WorkerWatcher, stopCh chan struct{}, cfg *pool.Config) *dynAllocator {
	return &dynAllocator{
		maxWorkers:  cfg.DynamicAllocatorOpts.MaxWorkers,
		spawnRate:   cfg.DynamicAllocatorOpts.SpawnRate,
		idleTimeout: cfg.DynamicAllocatorOpts.IdleTimeout,
		baseWorkers: cfg.NumWorkers,
		ww:          ww,
		log:         log,
		stopCh:      stopCh,
		rateLimit:   ratelimiter.NewRateLimiter(time.Second),
	}
}

// dynWorkers is the number of workers above the base pool size.
func (da *dynAllocator) dynWorkers() uint64 {
	n := da.ww.NumWorkers()
	if n <= da.baseWorkers {
		return 0
	}

	return n - da.baseWorkers
}

type spawnResult struct {
	added uint64
	// the call was rejected by the rate limiter
	rateLimited bool
}

// addMoreWorkers spawns one batch of dynamic workers.
func (da *dynAllocator) addMoreWorkers() *spawnResult {
	select {
	case <-da.stopCh:
		return nil
	default:
	}

	// signal allocation pressure even when rate limited, so the TTL listener does not
	// deallocate while triggers keep arriving
	da.lastAllocTry.Store(time.Now().UnixNano())

	if !da.rateLimit.TryAcquire() {
		da.log.Warn("rate limit exceeded for dynamic allocation, skipping")
		return &spawnResult{rateLimited: true}
	}

	// return the token after the cooldown
	defer da.rateLimit.Release()

	da.mu.Lock()
	defer da.mu.Unlock()

	da.log.Debug("no free workers, trying to allocate dynamically",
		"idle_timeout", da.idleTimeout,
		"max_workers", da.maxWorkers,
		"spawn_rate", da.spawnRate)

	if !da.started {
		da.startIdleTTLListener()
		da.started = true
	}

	if da.dynWorkers() >= da.maxWorkers {
		da.log.Warn("can't allocate more workers, already allocated max workers", "max_workers", da.maxWorkers)
		return nil
	}

	added := uint64(0)
	for range da.spawnRate {
		// spawn as many workers as the user specified in the spawn rate configuration, but not more than max workers
		if da.dynWorkers() >= da.maxWorkers {
			break
		}

		err := da.ww.AddWorker()
		if err != nil {
			// AddWorker already retried for the whole allocate timeout; giving up on the
			// batch keeps the lock hold time bounded
			da.log.Error("failed to allocate worker", "error", err)
			break
		}

		added++
		da.log.Debug("allocated additional worker", "dynamically allocated", da.dynWorkers())
	}

	if added == 0 {
		return nil
	}

	return &spawnResult{added: added}
}

func (da *dynAllocator) startIdleTTLListener() {
	da.log.Debug("starting dynamic allocator listener", "idle_timeout", da.idleTimeout)
	go func() {
		triggerTTL := time.Tick(da.idleTimeout)

		for {
			select {
			case <-da.stopCh:
				da.mu.Lock()
				da.started = false
				da.mu.Unlock()
				da.log.Debug("dynamic allocator listener stopped")
				return
			case <-triggerTTL:
				// postpone deallocation while allocation pressure is present
				last := da.lastAllocTry.Load()
				if last != 0 && time.Since(time.Unix(0, last)) < da.idleTimeout {
					da.log.Debug("skipping deallocation of dynamic workers, recent allocation detected")
					continue
				}

				da.mu.Lock()

				dyn := da.dynWorkers()
				if dyn == 0 {
					// the flag flips under mu, so a concurrent addMoreWorkers observes it
					// only after this listener is gone and starts a fresh one
					da.started = false
					da.mu.Unlock()
					da.log.Debug("dynamic allocator listener exited, no dynamic workers left")
					return
				}

				// remove one spawnRate-sized batch per tick, and only workers that are idle
				// right now: a fully busy pool is left untouched
				batch := min(dyn, da.spawnRate, uint64(da.ww.FreeWorkers())) //nolint:gosec
				if batch == 0 {
					da.mu.Unlock()
					da.log.Debug("skipping deallocation, no idle workers at the moment")
					continue
				}

				da.log.Debug("deallocating dynamic workers", "batch", batch, "dynamically allocated", dyn)

				for range batch {
					// bounded wait: only a worker that becomes free within the window is removed
					ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*500)
					err := da.ww.RemoveWorker(ctx)
					cancel()
					if err != nil {
						// remaining workers are busy again (or the watcher is stopping)
						da.log.Debug("stopping deallocation batch", "error", err)
						break
					}

					da.log.Debug("deallocated additional worker", "dynamically allocated", da.dynWorkers())
				}

				if da.dynWorkers() > 0 {
					da.mu.Unlock()
					da.log.Debug("dynamic allocator listener continuing, still have dynamic workers", "remaining", da.dynWorkers())
					continue
				}

				da.started = false
				da.mu.Unlock()
				da.log.Debug("dynamic allocator listener exited, all dynamic workers deallocated")
				return
			}
		}
	}()
}
