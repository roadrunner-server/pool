package static_pool //nolint:stylecheck

import (
	"context"
	stderr "errors"
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

	if !*da.started.Load() {
		// start the dynamic allocator listener
		da.dynamicTTLListener()
		da.started.Store(p(true))
	} else {
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

	// reset the TTL listener
	select {
	case da.ttlTriggerChan <- struct{}{}:
	case <-time.After(time.Minute):
		return nil, errors.E(op, stderr.New("failed to reset the TTL listener"))
	}

	// if we already allocated max workers, we can't allocate more
	if *da.currAllocated.Load() >= da.maxWorkers {
		// can't allocate more
		return nil, errors.E(op, stderr.New("can't allocate more workers, increase max_workers option (max_workers limit is 100)"))
	}

	// if we have dynamic allocator, we can try to allocate new worker
	// this worker should not be released here, but instead will be released in the Exec function
	err := da.ww.AddWorker()
	if err != nil {
		da.log.Error("failed to allocate dynamic worker", zap.Error(err))
		return nil, errors.E(op, err)
	}

	// increase number of additionally allocated options
	_ = da.currAllocated.Swap(p(*da.currAllocated.Load() + 1))

	da.log.Debug("allocated additional worker", zap.Uint64("currently additionally allocated", *da.currAllocated.Load()))

	// we starting from the 1 because we already allocated one worker which would be released in the Exec function
	for i := uint64(1); i <= da.spawnRate; i++ {
		// spawn as much workers as user specified in the spawn rate configuration, but not more than max workers
		if *da.currAllocated.Load() >= da.maxWorkers {
			break
		}

		err = da.ww.AddWorker()
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

				for i := *da.currAllocated.Load(); i > 0; i-- {
					// take the worker from the stack, inifinite timeout
					// we should not block here forever
					err := da.ww.RemoveWorker(context.Background())
					if err != nil {
						da.log.Error("failed to take worker from the stack", zap.Error(err))
						continue
					}

					// reset the number of allocated workers
					// potential problem: if we'd have an error in the da.ww.Take code block, we'd still have the currAllocated > 0

					// decrease number of additionally allocated options
					_ = da.currAllocated.Swap(p(*da.currAllocated.Load() - 1))
				}

				if *da.currAllocated.Load() != 0 {
					da.log.Error("failed to deallocate all dynamically allocated workers", zap.Uint64("remaining", *da.currAllocated.Load()))
				}

				da.mu.Unlock()
				da.execLock.Unlock()
				triggerTTL.Stop()
				goto exit

				// when this channel is triggered, we should extend the TTL of all dynamically allocated workers
			case <-da.ttlTriggerChan:
				triggerTTL.Reset(da.idleTimeout)
			}
		}
	exit:
		da.started.Store(p(false))
	}()
}

func p[T any](val T) *T {
	return &val
}
