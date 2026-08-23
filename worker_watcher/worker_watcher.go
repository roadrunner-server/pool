package worker_watcher

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"log/slog"

	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/events"
	"github.com/roadrunner-server/pool/v2/fsm"
	"github.com/roadrunner-server/pool/v2/worker"
	"github.com/roadrunner-server/pool/v2/worker_watcher/container/channel"
)

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

	log *slog.Logger

	allocator       Allocator
	allocateTimeout time.Duration
	stopCh          chan struct{}
	stopOnce        sync.Once
	destroyed       atomic.Bool
}

// NewSyncWorkerWatcher is a constructor for the Watcher
func NewSyncWorkerWatcher(allocator Allocator, log *slog.Logger, numWorkers uint64, allocateTimeout time.Duration) *WorkerWatcher {
	eb, _ := events.NewEventBus()
	ww := &WorkerWatcher{
		container:       channel.NewVector(),
		log:             log,
		eventBus:        eb,
		allocateTimeout: allocateTimeout,
		workers:         sync.Map{},
		allocator:       allocator,
		stopCh:          make(chan struct{}),
	}

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

// NumWorkers returns the live number of workers tracked by the watcher.
func (ww *WorkerWatcher) NumWorkers() uint64 {
	return ww.numWorkers.Load()
}

// FreeWorkers returns the number of workers currently idling in the container.
func (ww *WorkerWatcher) FreeWorkers() int {
	return ww.container.Len()
}

func (ww *WorkerWatcher) AddWorker() error {
	ww.mu.Lock()
	defer ww.mu.Unlock()

	if ww.numWorkers.Load() >= channel.MaxWorkers {
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

// Take returns a worker in the Ready state from the container
func (ww *WorkerWatcher) Take(ctx context.Context) (*worker.Process, error) {
	const op = errors.Op("worker_watcher_get_free_worker")
	for {
		w, err := ww.container.Pop(ctx)
		if err != nil {
			if errors.Is(errors.WatcherStopped, err) {
				return nil, errors.E(op, errors.WatcherStopped)
			}

			return nil, errors.E(op, err)
		}

		switch w.State().CurrentState() {
		case fsm.StateReady:
			return w, nil
		case fsm.StateWorking:
			// put it back, let the worker finish the work
			ww.container.Push(w)
			continue
		default:
			// the worker does no work while in the container, so an unready one (TTL-ed or
			// inconsistent) is safe to kill
			_ = w.Kill()
			continue
		}
	}
}

func (ww *WorkerWatcher) Allocate() error {
	const op = errors.Op("worker_watcher_allocate_new")

	sw, err := ww.allocator()
	if err != nil {
		// log incident
		ww.log.Error("allocate", "error", err)
		// if no timeout, return the error immediately
		if ww.allocateTimeout == 0 {
			return errors.E(op, errors.WorkerAllocate, err)
		}

		// retry every second until the allocate timeout elapses
		allocateFreq := time.Tick(time.Second)
		tt := time.After(ww.allocateTimeout)
		for {
			select {
			case <-tt:
				// timeout exceeds, worker can't be allocated
				return errors.E(op, errors.WorkerAllocate, err)

			case <-allocateFreq:
				sw, err = ww.allocator()
				if err != nil {
					// log incident
					ww.log.Error("allocate retry attempt failed", "internal_event_name", events.EventWorkerError.String(), "error", err)
					continue
				}

				// reallocated
				goto done

			case <-ww.stopCh:
				return errors.E(op, errors.WatcherStopped)
			}
		}
	}

done:
	// the watcher may have been destroyed while the worker was being spawned; a worker
	// added past this point would never be stopped
	select {
	case <-ww.stopCh:
		go func() {
			_ = sw.Wait()
		}()
		_ = sw.Kill()
		return errors.E(op, errors.WatcherStopped)
	default:
	}

	// add worker to Wait
	ww.addToWatch(sw)
	// add a new worker to the worker's slice (to get information about workers in parallel)
	if w, ok := ww.workers.Swap(sw.Pid(), sw); ok {
		ww.log.Warn("allocated worker already exists, killing duplicate, report this case", "pid", sw.Pid())
		_ = w.(*worker.Process).Kill()
	}
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
			ww.log.Debug("worker release", "error", err)
		}
	default:
		// in all other cases, we have no choice rather than kill the worker
		_ = w.Kill()
	}
}

// stopWatchedWorkers asks every tracked worker to shut down; Process.Stop waits out its own
// grace period before killing one that does not answer. The caller must hold ww.mu.
func (ww *WorkerWatcher) stopWatchedWorkers() {
	ww.disposeWatchedWorkers((*worker.Process).Stop)
}

// killWatchedWorkers terminates every tracked worker at once, for when there is no time left
// to negotiate. The caller must hold ww.mu.
func (ww *WorkerWatcher) killWatchedWorkers() {
	ww.disposeWatchedWorkers((*worker.Process).Kill)
}

func (ww *WorkerWatcher) disposeWatchedWorkers(dispose func(*worker.Process) error) {
	wg := &sync.WaitGroup{}
	ww.workers.Range(func(key, value any) bool {
		w := value.(*worker.Process)
		wg.Go(func() {
			w.State().Transition(fsm.StateDestroyed)
			_ = dispose(w)
			// remove worker from the channel
			w.Callback()
		})

		ww.workers.Delete(key)
		return true
	})

	wg.Wait()
}

func (ww *WorkerWatcher) Reset(ctx context.Context) uint64 {
	// do not release new workers
	ww.container.Reset()
	tt := time.Tick(time.Second)
	for {
		select {
		case <-tt:
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
			ww.stopWatchedWorkers()
			ww.container.ResetDone()

			// todo: rustatian, do we need this mutex?
			ww.mu.Unlock()

			return ww.numWorkers.Load()
		case <-ctx.Done():
			ww.mu.Lock()
			ww.killWatchedWorkers()
			ww.container.ResetDone()
			ww.mu.Unlock()

			return ww.numWorkers.Load()
		}
	}
}

// Destroy all underlying containers (but let them complete the task)
func (ww *WorkerWatcher) Destroy(ctx context.Context) {
	if ww.destroyed.Load() {
		return
	}
	ww.stopOnce.Do(func() {
		close(ww.stopCh)
	})
	ww.mu.Lock()
	// do not release new workers
	ww.container.Destroy()
	ww.mu.Unlock()
	// destroy container; we don't use ww mutex here, since we should be able to push worker
	tt := time.Tick(time.Second)
	for {
		select {
		case <-tt:
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
			ww.stopWatchedWorkers()
			ww.numWorkers.Store(0)
			ww.destroyed.Store(true)
			ww.mu.Unlock()
			return
		case <-ctx.Done():
			ww.log.Debug("destroy: context canceled", "error", ctx.Err())
			ww.mu.Lock()
			ww.killWatchedWorkers()
			ww.numWorkers.Store(0)
			ww.destroyed.Store(true)
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

	base := make([]*worker.Process, 0, ww.numWorkers.Load())
	ww.workers.Range(func(key, value any) bool {
		base = append(base, value.(*worker.Process))
		return true
	})

	return base
}

func (ww *WorkerWatcher) wait(w *worker.Process) {
	err := w.Wait()
	if err != nil {
		ww.log.Debug("worker stopped", "internal_event_name", events.EventWorkerWaitExit.String(), "error", err)
	}

	// remove worker
	ww.workers.Delete(w.Pid())

	if w.State().Compare(fsm.StateDestroyed) {
		// worker was manually destroyed, no need to replace
		if err != nil {
			ww.log.Debug("worker destroyed", "pid", w.Pid(), "internal_event_name", events.EventWorkerDestruct.String(), "error", err)
		} else {
			ww.log.Debug("worker destroyed", "pid", w.Pid(), "internal_event_name", events.EventWorkerDestruct.String())
		}
		return
	}

	err = ww.Allocate()
	if err != nil {
		// the watcher is shutting down and the dead worker gets no replacement; drop it
		// from the count so Destroy's drain can converge
		if errors.Is(errors.WatcherStopped, err) {
			for {
				n := ww.numWorkers.Load()
				if n == 0 || ww.numWorkers.CompareAndSwap(n, n-1) {
					break
				}
			}
			return
		}

		// dead worker was not replaced; saturate at zero so a concurrent removal cannot
		// wrap the counter
		for {
			n := ww.numWorkers.Load()
			if n == 0 || ww.numWorkers.CompareAndSwap(n, n-1) {
				break
			}
		}
		ww.log.Error("failed to allocate the worker", "internal_event_name", events.EventWorkerError.String(), "error", err)
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
