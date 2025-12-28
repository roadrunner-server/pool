package worker_watcher

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/events"
	"github.com/roadrunner-server/pool/fsm"
	"github.com/roadrunner-server/pool/worker"
	"github.com/roadrunner-server/pool/worker_watcher/container/channel"
	"go.uber.org/zap"
)

const maxWorkers = 500

// Allocator is responsible for worker allocation in the pool
type Allocator func() (*worker.Process, error)

type WorkerWatcher struct {
	mu sync.RWMutex
	// actually don't have a lot of impl here, so interface not needed
	container *channel.Vec
	// used to control Destroy stage (that all workers are in the container)
	numWorkers atomic.Uint64
	eventBus   *events.Bus

	// map with the worker's pointers
	workers sync.Map
	// workers map[int64]*worker.Process

	log *zap.Logger

	allocator       Allocator
	allocateTimeout time.Duration
}

// NewSyncWorkerWatcher is a constructor for the Watcher
func NewSyncWorkerWatcher(allocator Allocator, log *zap.Logger, numWorkers uint64, allocateTimeout time.Duration) *WorkerWatcher {
	eb, _ := events.NewEventBus()
	ww := &WorkerWatcher{
		container:       channel.NewVector(),
		log:             log,
		eventBus:        eb,
		allocateTimeout: allocateTimeout,
		workers:         sync.Map{},
		allocator:       allocator,
	}

	// pass a ptr to the number of workers to avoid blocking in the TTL loop
	ww.numWorkers.Store(numWorkers)
	return ww
}

func (ww *WorkerWatcher) Watch(workers []*worker.Process) error {
	ww.mu.Lock()
	defer ww.mu.Unlock()

	// else we can add all workers
	for i := range workers {
		ww.container.Push(workers[i])
		// add worker to watch slice
		ww.workers.Store(workers[i].Pid(), workers[i])
		ww.addToWatch(workers[i])
	}

	return nil
}

func (ww *WorkerWatcher) AddWorker() error {
	ww.mu.Lock()
	defer ww.mu.Unlock()

	if ww.numWorkers.Load() >= maxWorkers {
		return errors.E(errors.WorkerAllocate, errors.Str("container is full, maximum number of workers reached"))
	}

	err := ww.Allocate()
	if err != nil {
		return err
	}

	ww.numWorkers.Add(1)
	return nil
}

func (ww *WorkerWatcher) RemoveWorker(ctx context.Context) error {
	ww.mu.Lock()
	defer ww.mu.Unlock()

	// can't remove the last worker
	if ww.numWorkers.Load() == 1 {
		ww.log.Warn("can't remove the last worker")
		return nil
	}

	w, err := ww.Take(ctx)
	if err != nil {
		return err
	}

	// destroy and stop
	w.State().Transition(fsm.StateDestroyed)
	_ = w.Stop()

	ww.numWorkers.Add(^uint64(0))
	ww.workers.Delete(w.Pid())

	return nil
}

// Take is not a thread-safe operation
func (ww *WorkerWatcher) Take(ctx context.Context) (*worker.Process, error) {
	const op = errors.Op("worker_watcher_get_free_worker")
	// we need lock here to prevent Pop operation when ww in the resetting state
	// thread safe operation
	w, err := ww.container.Pop(ctx)
	if err != nil {
		if errors.Is(errors.WatcherStopped, err) {
			return nil, errors.E(op, errors.WatcherStopped)
		}

		return nil, errors.E(op, err)
	}

	// fast path, worker not nil and in the ReadyState
	if w.State().Compare(fsm.StateReady) {
		return w, nil
	}

	// =========================================================
	// SLOW PATH
	_ = w.Kill()
	// no free workers in the container or worker not in the ReadyState (TTL-ed)
	// try to continuously get a free one
	for {
		w, err = ww.container.Pop(ctx)
		if err != nil {
			if errors.Is(errors.WatcherStopped, err) {
				return nil, errors.E(op, errors.WatcherStopped)
			}
			return nil, errors.E(op, err)
		}

		switch w.State().CurrentState() {
		// return only workers in the Ready state
		// check first
		case fsm.StateReady:
			return w, nil
		case fsm.StateWorking: // how??
			ww.container.Push(w) // put it back, let the worker finish the work
			continue
		default:
			// worker doing no work because it in the container
			// so we can safely kill it (inconsistent state)
			_ = w.Stop()
			// try to get new worker
			continue
		}
	}
}

func (ww *WorkerWatcher) Allocate() error {
	const op = errors.Op("worker_watcher_allocate_new")

	sw, err := ww.allocator()
	if err != nil {
		// log incident
		ww.log.Error("allocate", zap.Error(err))
		// if no timeout, return the error immediately
		if ww.allocateTimeout == 0 {
			return errors.E(op, errors.WorkerAllocate, err)
		}

		// every second
		allocateFreq := time.NewTicker(time.Second)

		tt := time.After(ww.allocateTimeout)
		for {
			select {
			case <-tt:
				// reduce the number of workers
				ww.numWorkers.Add(^uint64(0))
				allocateFreq.Stop()
				// timeout exceeds, worker can't be allocated
				return errors.E(op, errors.WorkerAllocate, err)

			case <-allocateFreq.C:
				sw, err = ww.allocator()
				if err != nil {
					// log incident
					ww.log.Error("allocate retry attempt failed", zap.String("internal_event_name", events.EventWorkerError.String()), zap.Error(err))
					continue
				}

				// reallocated
				allocateFreq.Stop()
				goto done
			}
		}
	}

done:
	// add worker to Wait
	ww.addToWatch(sw)
	// add a new worker to the worker's slice (to get information about workers in parallel)
	if w, ok := ww.workers.LoadAndDelete(sw.Pid()); ok {
		ww.log.Warn("allocated worker already exists, killing duplicate, report this case", zap.Int64("pid", sw.Pid()))
		_ = w.(*worker.Process).Kill()
	}

	ww.workers.Store(sw.Pid(), sw)
	// push the worker to the container
	ww.Release(sw)
	return nil
}

// Release O(1) operation
func (ww *WorkerWatcher) Release(w *worker.Process) {
	switch w.State().CurrentState() {
	case fsm.StateReady:
		ww.container.Push(w)
	case
		// all the possible wrong states, when we can send a stop signal
		fsm.StateInactive,
		fsm.StateDestroyed,
		fsm.StateErrored,
		fsm.StateWorking,
		fsm.StateInvalid,
		fsm.StateMaxMemoryReached,
		fsm.StateMaxJobsReached,
		fsm.StateIdleTTLReached,
		fsm.StateTTLReached,
		fsm.StateExecTTLReached:

		err := w.Stop()
		if err != nil {
			ww.log.Debug("worker release", zap.Error(err))
		}
	default:
		// in all other cases, we have no choice rather than kill the worker
		_ = w.Kill()
	}
}

func (ww *WorkerWatcher) Reset(ctx context.Context) uint64 {
	// do not release new workers
	ww.container.Reset()
	tt := time.NewTicker(time.Second)
	defer tt.Stop()
	for {
		select {
		case <-tt.C:
			ww.mu.RLock()

			// that might be one of the workers is working. To proceed, all workers should be inside a channel
			if ww.numWorkers.Load() != uint64(ww.container.Len()) { //nolint:gosec
				ww.mu.RUnlock()
				continue
			}
			ww.mu.RUnlock()
			// All workers at this moment are in the container
			// Pop operation is blocked; push can't be done, since it's not possible to pop
			ww.mu.Lock()

			wg := &sync.WaitGroup{}
			ww.workers.Range(func(key, value any) bool {
				w := value.(*worker.Process)
				wg.Go(func() {
					w.State().Transition(fsm.StateDestroyed)
					// kill the worker
					_ = w.Stop()
					// remove worker from the channel
					w.Callback()
				})

				ww.workers.Delete(key)
				return true
			})

			wg.Wait()

			ww.container.ResetDone()

			// todo: rustatian, do we need this mutex?
			ww.mu.Unlock()

			return ww.numWorkers.Load()
		case <-ctx.Done():
			// kill workers
			ww.mu.Lock()
			// drain workers slice
			wg := &sync.WaitGroup{}

			ww.workers.Range(func(key, value any) bool {
				w := value.(*worker.Process)
				wg.Go(func() {
					w.State().Transition(fsm.StateDestroyed)
					// kill the worker
					_ = w.Stop()
					// remove worker from the channel
					w.Callback()
				})

				ww.workers.Delete(key)
				return true
			})

			wg.Wait()
			ww.container.ResetDone()
			ww.mu.Unlock()

			return ww.numWorkers.Load()
		}
	}
}

// Destroy all underlying containers (but let them complete the task)
func (ww *WorkerWatcher) Destroy(ctx context.Context) {
	ww.mu.Lock()
	// do not release new workers
	ww.container.Destroy()
	ww.mu.Unlock()

	tt := time.NewTicker(time.Second * 1)
	// destroy container; we don't use ww mutex here, since we should be able to push worker
	defer tt.Stop()
	for {
		select {
		case <-tt.C:
			ww.mu.RLock()
			// that might be one of the workers is working
			if ww.numWorkers.Load() != uint64(ww.container.Len()) { //nolint:gosec
				ww.mu.RUnlock()
				continue
			}

			ww.mu.RUnlock()
			// All workers at this moment are in the container
			// Pop operation is blocked, push can't be done, since it's not possible to pop

			ww.mu.Lock()
			// drain a channel, this operation will not actually pop, only drain a channel
			_, _ = ww.container.Pop(ctx)
			wg := &sync.WaitGroup{}

			ww.workers.Range(func(key, value any) bool {
				w := value.(*worker.Process)
				wg.Go(func() {
					w.State().Transition(fsm.StateDestroyed)
					// kill the worker
					_ = w.Stop()
					// remove worker from the channel
					w.Callback()
				})

				ww.workers.Delete(key)
				return true
			})
			wg.Wait()

			ww.mu.Unlock()
			return
		case <-ctx.Done():
			// kill workers
			ww.mu.Lock()
			wg := &sync.WaitGroup{}
			ww.workers.Range(func(key, value any) bool {
				w := value.(*worker.Process)
				wg.Go(func() {
					w.State().Transition(fsm.StateDestroyed)
					// kill the worker
					_ = w.Stop()
					// remove worker from the channel
					w.Callback()
				})
				ww.workers.Delete(key)
				return true
			})

			wg.Wait()

			ww.mu.Unlock()
			return
		}
	}
}

// List - this is O(n) operation, and it will return copy of the actual workers
func (ww *WorkerWatcher) List() []*worker.Process {
	if ww.numWorkers.Load() == 0 {
		return nil
	}

	base := make([]*worker.Process, 0, 2)

	ww.workers.Range(func(key, value any) bool {
		base = append(base, value.(*worker.Process))
		return true
	})

	return base
}

func (ww *WorkerWatcher) wait(w *worker.Process) {
	err := w.Wait()
	if err != nil {
		ww.log.Debug("worker stopped", zap.String("internal_event_name", events.EventWorkerWaitExit.String()), zap.Error(err))
	}

	// remove worker
	ww.workers.Delete(w.Pid())

	if w.State().Compare(fsm.StateDestroyed) {
		// worker was manually destroyed, no need to replace
		ww.log.Debug("worker destroyed", zap.Int64("pid", w.Pid()), zap.String("internal_event_name", events.EventWorkerDestruct.String()), zap.Error(err))
		return
	}

	err = ww.Allocate()
	if err != nil {
		ww.log.Error("failed to allocate the worker", zap.String("internal_event_name", events.EventWorkerError.String()), zap.Error(err))
		if ww.numWorkers.Load() == 0 {
			panic("no workers available, can't run the application")
		}

		return
	}

	// this event used mostly for the temporal plugin
	ww.eventBus.Send(events.NewEvent(events.EventWorkerStopped, "worker_watcher", fmt.Sprintf("process exited, pid: %d", w.Pid())))
}

func (ww *WorkerWatcher) addToWatch(wb *worker.Process) {
	// this callback is used to remove the bad workers from the container
	wb.AddCallback(func() {
		ww.container.Remove()
	})
	go func() {
		ww.wait(wb)
	}()
}
