package static_pool //nolint:stylecheck

import (
	"context"
	stderr "errors"
	"github.com/roadrunner-server/pool/fsm"
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
	threshold   uint64

	// internal
	currAllocated  atomic.Pointer[uint64]
	mu             *sync.Mutex
	execLock       *sync.RWMutex
	ttlTriggerChan chan struct{}
	started        atomic.Pointer[bool]
	log            *zap.Logger
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
	execLock *sync.RWMutex,
	cfg *pool.Config) *dynAllocator {
	da := &dynAllocator{
		maxWorkers:     cfg.DynamicAllocatorOpts.MaxWorkers,
		spawnRate:      cfg.DynamicAllocatorOpts.SpawnRate,
		idleTimeout:    cfg.DynamicAllocatorOpts.IdleTimeout,
		threshold:      cfg.DynamicAllocatorOpts.Threshold,
		mu:             &sync.Mutex{},
		ttlTriggerChan: make(chan struct{}, 1),
		ww:             ww,
		execLock:       execLock,
		allocator:      alloc,
		log:            log,
		stopCh:         stopCh,
	}

	da.currAllocated.Store(p(uint64(0)))
	da.started.Store(p(false))

	return da
}

func (da *dynAllocator) allocateDynamically() (*worker.Process, error) {
	const op = errors.Op("allocate_dynamically")

	// obtain an operation lock
	// we can use a lock-free approach here, but it's not necessary
	da.mu.Lock()
	defer da.mu.Unlock()

	da.log.Debug("Checking if we need to allocate workers dynamically",
		zap.Duration("idle_timeout", da.idleTimeout),
		zap.Uint64("max_workers", da.maxWorkers),
		zap.Uint64("spawn_rate", da.spawnRate),
		zap.Uint64("threshold", da.threshold))

	if !*da.started.Load() {
		// start the dynamic allocator listener
		da.dynamicTTLListener()
		da.started.Store(p(true))
	} else {
		da.log.Debug("dynamic allocator listener already started, trying to allocate worker immediately with 2s timeout")
		// if the listener was started we can try to get the worker with a very short timeout, which was probably allocated by the previous NoFreeWorkers error
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*2)
		w, err := da.ww.Take(ctx)
		cancel()
		if err != nil {
			if errors.Is(errors.NoFreeWorkers, err) {
				goto allocate
			}

			return nil, errors.E(op, err)
		}

		return w, nil
	}

allocate:
	// otherwise, we can try to allocate a new batch of workers

	// if we already allocated max workers, we can't allocate more
	if *da.currAllocated.Load() >= da.maxWorkers {
		// can't allocate more
		return nil, errors.E(op, stderr.New("can't allocate more workers, increase max_workers option (max_workers limit is 100)"))
	}

	// we starting from the 1 because we already allocated one worker which would be released in the Exec function
	// i < da.spawnRate - we can't allocate more workers than the spawn rate
	for i := uint64(0); i < da.spawnRate; i++ {
		// spawn as much workers as user specified in the spawn rate configuration, but not more than max workers
		if *da.currAllocated.Load() >= da.maxWorkers {
			break
		}

		err := da.ww.AddWorker()
		if err != nil {
			return nil, errors.E(op, err)
		}

		// reset ttl after every alloated worker
		select {
		case da.ttlTriggerChan <- struct{}{}:
		case <-time.After(time.Minute):
			return nil, errors.E(op, stderr.New("failed to reset the TTL listener"))
		}

		// increase number of additionally allocated options
		_ = da.currAllocated.Swap(p(*da.currAllocated.Load() + 1))
		da.log.Debug("allocated additional worker", zap.Uint64("currently additionally allocated", *da.currAllocated.Load()))
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
				da.log.Debug("dynamic workers TTL", zap.String("reason", "idle timeout reached"))
				// get the Exec (the whole operation) lock
				da.execLock.Lock()
				// get the DynamicAllocatorOpts lock to prevent operations on the CurrAllocated
				da.mu.Lock()

				// if we don't have any dynamically allocated workers, we can skip the deallocation
				if *da.currAllocated.Load() == 0 {
					da.mu.Unlock()
					da.execLock.Unlock()
					goto exit
				}

				workers := da.ww.List()

				// Get the total number of workers
				totalWorkers := len(workers)
				if totalWorkers == 0 {
					da.mu.Unlock()
					da.execLock.Unlock()
					goto exit
				}

				// Calculate how many workers we can deallocate without going below the threshold
				// We need to ensure that after deallocation, the percentage of free workers is still above the threshold
				// First, estimate the number of free workers (this is an approximation)
				// We know that dynamically allocated workers are likely free, so we'll use that as a starting point
				var freeWorkers uint64 = 0
				for _, w := range workers {
					if w.State().Compare(fsm.StateReady) {
						freeWorkers++
					}
				}

				// Calculate the percentage of free workers after potential deallocation
				// We want to ensure: (freeWorkers - workersToRemove) / totalWorkers >= threshold / 100
				// Solving for workersToRemove: workersToRemove <= freeWorkers - (threshold * totalWorkers / 100)
				minFreeWorkersNeeded := (da.threshold * uint64(totalWorkers)) / 100

				var workersToRemove uint64
				if freeWorkers > minFreeWorkersNeeded {
					workersToRemove = freeWorkers - minFreeWorkersNeeded
				} else {
					// If we're already at or below the threshold, don't remove any workers
					workersToRemove = 0
				}

				da.log.Debug("calculating workers to remove",
					zap.Uint64("total_workers", uint64(totalWorkers)),
					zap.Uint64("estimated_free_workers", freeWorkers),
					zap.Uint64("min_free_workers_needed", minFreeWorkersNeeded),
					zap.Uint64("workers_to_remove", workersToRemove),
					zap.Uint64("threshold", da.threshold))

				// Only remove workers if we need to
				if workersToRemove > 0 {
					// Limit to the number of dynamically allocated workers
					if workersToRemove > *da.currAllocated.Load() {
						workersToRemove = *da.currAllocated.Load()
					}

					for i := uint64(0); i < workersToRemove; i++ {
						// take the worker from the stack, infinite timeout
						// we should not block here forever
						err := da.ww.RemoveWorker(context.Background())
						if err != nil {
							da.log.Error("failed to take worker from the stack", zap.Error(err))
							continue
						}

						// decrease number of additionally allocated options
						_ = da.currAllocated.Swap(p(*da.currAllocated.Load() - 1))
						da.log.Debug("deallocated additional worker", zap.Uint64("currently additionally allocated", *da.currAllocated.Load()))
					}
				} else {
					da.log.Debug("not removing any workers to maintain threshold",
						zap.Uint64("threshold", da.threshold),
						zap.Uint64("current_allocated", *da.currAllocated.Load()))
				}

				// Only log an error if we intended to remove all workers but failed
				if workersToRemove == *da.currAllocated.Load() && *da.currAllocated.Load() != 0 {
					da.log.Error("failed to deallocate all intended workers", zap.Uint64("remaining", *da.currAllocated.Load()))
				} else if *da.currAllocated.Load() != 0 {
					da.log.Debug("keeping some dynamically allocated workers to maintain threshold", zap.Uint64("remaining", *da.currAllocated.Load()))
				}

				da.mu.Unlock()
				da.execLock.Unlock()
				triggerTTL.Stop()
				goto exit

				// when this channel is triggered, we should extend the TTL of all dynamically allocated workers
			case <-da.ttlTriggerChan:
				da.log.Debug("TTL trigger received, extending TTL of all dynamically allocated workers")
				triggerTTL.Reset(da.idleTimeout)
			}
		}
	exit:
		da.started.Store(p(false))
		da.log.Debug("dynamic allocator listener exited, all dynamically allocated workers deallocated")
	}()
}

func p[T any](val T) *T {
	return &val
}
