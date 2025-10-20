// Dynamic allocator for the static pool implementation
// It allocates new workers with batch spawn rate when there are no free workers
// It uses 2 functions: allocateDynamically to allocate new workers and dynamicTTLListener
package static_pool

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/pool/pool"
	"github.com/roadrunner-server/pool/worker"
	"github.com/roadrunner-server/pool/worker_watcher"
	"go.uber.org/zap"
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
	log           *zap.Logger
	// pool
	ww        *worker_watcher.WorkerWatcher
	allocator func() (*worker.Process, error)
	stopCh    chan struct{}
}

func newDynAllocator(
	log *zap.Logger,
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
	}

	da.currAllocated.Store(0)
	da.started.Store(false)

	return da
}

func (da *dynAllocator) allocateDynamically() (*worker.Process, error) {
	const op = errors.Op("allocate_dynamically")

	// operation lock
	// we can use a lock-free approach here, but it's not necessary
	da.mu.Lock()
	defer da.mu.Unlock()

	da.log.Debug("No free workers, trying to allocate dynamically",
		zap.Duration("idle_timeout", da.idleTimeout),
		zap.Uint64("max_workers", da.maxWorkers),
		zap.Uint64("spawn_rate", da.spawnRate))

	if !da.started.Load() {
		// start the dynamic allocator listener
		da.dynamicTTLListener()
		da.started.Store(true)
	}

	// if we already allocated max workers, we can't allocate more
	if da.currAllocated.Load() >= da.maxWorkers {
		// can't allocate more
		return nil, errors.E(op, fmt.Errorf("can't allocate more workers, increase max_workers option (max_workers limit is %d)", da.maxWorkers), errors.NoFreeWorkers)
	}

	// we're starting from the 1 because we already allocated one worker which would be released in the Exec function
	// i < da.spawnRate - we can't allocate more workers than the spawn rate
	for i := uint64(0); i < da.spawnRate; i++ {
		// spawn as many workers as the user specified in the spawn rate configuration, but not more than max workers
		if da.currAllocated.Load() >= da.maxWorkers {
			break
		}

		err := da.ww.AddWorker()
		if err != nil {
			return nil, errors.E(op, err)
		}

		// increase the number of additionally allocated options
		aw := da.currAllocated.Add(1)
		da.log.Debug("allocated additional worker", zap.Uint64("currently additionally allocated", aw))
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*2)
	w, err := da.ww.Take(ctx)
	cancel()

	return w, err
}

func (da *dynAllocator) dynamicTTLListener() {
	da.log.Debug("starting dynamic allocator listener", zap.Duration("idle_timeout", da.idleTimeout))
	go func() {
		// DynamicAllocatorOpts are read-only, so we can use them without a lock
		triggerTTL := time.NewTicker(da.idleTimeout)
		for {
			select {
			case <-da.stopCh:
				da.log.Debug("dynamic allocator listener stopped")
				goto exit
			// when this channel is triggered, we should deallocate all dynamically allocated workers
			case <-triggerTTL.C:
				triggerTTL.Stop()
				da.log.Debug("dynamic workers TTL", zap.String("reason", "idle timeout reached"))
				// get the DynamicAllocatorOpts lock to prevent operations on the CurrAllocated
				da.mu.Lock()

				// if we don't have any dynamically allocated workers, we can skip the deallocation
				if da.currAllocated.Load() == 0 {
					da.mu.Unlock()
					goto exit
				}

				alloc := da.currAllocated.Load()
				for range alloc {
					// take the worker from the stack, inifinite timeout
					// we should not block here forever
					err := da.ww.RemoveWorker(context.Background())
					if err != nil {
						da.log.Error("failed to take worker from the stack", zap.Error(err))
						continue
					}

					// reset the number of allocated workers
					// potential problem: if we had an error in the da.ww.Take code block, we'd still have the currAllocated > 0

					// decrease the number of additionally allocated options
					nw := da.currAllocated.Add(^uint64(0))
					da.log.Debug("deallocated additional worker", zap.Uint64("currently additionally allocated", nw))
				}

				if da.currAllocated.Load() != 0 {
					da.log.Error("failed to deallocate all dynamically allocated workers", zap.Uint64("remaining", da.currAllocated.Load()))
				}

				da.mu.Unlock()
				goto exit
			}
		}
	exit:
		da.started.Store(false)
		da.log.Debug("dynamic allocator listener exited, all dynamically allocated workers deallocated")
	}()
}
