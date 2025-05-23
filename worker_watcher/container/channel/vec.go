package channel

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/pool/fsm"
	"github.com/roadrunner-server/pool/worker"
)

type Vec struct {
	rwm sync.RWMutex
	// destroy signal
	destroy uint64
	// reset signal
	reset uint64
	// channel with the workers
	workers chan *worker.Process
}

func NewVector() *Vec {
	vec := &Vec{
		destroy: 0,
		reset:   0,
		workers: make(chan *worker.Process, 600),
	}

	return vec
}

// Push is O(1) operation
// In case of TTL and full channel O(n) worst case, where n is len of the channel
func (v *Vec) Push(w *worker.Process) {
	// add remove callback
	select {
	case v.workers <- w:
		// default select branch is only possible when dealing with TTL
		// because in that case, workers in the v.workers channel can be TTL-ed and killed
		// but presenting in the channel
	default:
		// the channel is full
		_ = w.Kill()
	}
}

func (v *Vec) Len() int {
	return len(v.workers)
}

func (v *Vec) Pop(ctx context.Context) (*worker.Process, error) {
	// remove all workers and return
	if atomic.LoadUint64(&v.destroy) == 1 {
		return nil, errors.E(errors.WatcherStopped)
	}

	// wait for the reset to complete
	for atomic.CompareAndSwapUint64(&v.reset, 1, 1) {
		select {
		case <-ctx.Done():
		default:
			time.Sleep(time.Millisecond * 100)
		}
	}

	// used only for the TTL-ed workers
	v.rwm.RLock()
	select {
	case w := <-v.workers:
		v.rwm.RUnlock()
		return w, nil
	case <-ctx.Done():
		v.rwm.RUnlock()
		return nil, errors.E(ctx.Err(), errors.NoFreeWorkers)
	}
}

func (v *Vec) ResetDone() {
	atomic.StoreUint64(&v.reset, 0)
}

func (v *Vec) Reset() {
	atomic.StoreUint64(&v.reset, 1)
}

func (v *Vec) Destroy() {
	atomic.StoreUint64(&v.destroy, 1)
}

func (v *Vec) Remove() {
	// Stop Pop operations
	v.rwm.Lock()
	defer v.rwm.Unlock()

	/*
		we can be in the default branch by the following reasons:
		1. TTL is set with no requests during the TTL
		2. Violated Get <-> Release operation (how ??)
	*/

	for range len(v.workers) {
		/*
			We need to drain vector until we found a worker in the Invalid/Killing/Killed/etc states.
			BUT while we are draining the vector, some worker might be reallocated and pushed into the v.workers
			so, down by the code, we might have a problem when pushing the new worker to the v.workers
		*/
		wrk := <-v.workers

		switch wrk.State().CurrentState() {
		// good states
		case fsm.StateWorking, fsm.StateReady:
			// put the worker back
			// generally, while send and receive operations are concurrent (from the channel), channel behave
			// like a FIFO, but when re-sending from the same goroutine it behaves like a FILO
			select {
			case v.workers <- wrk:
				continue

				// all bad states are here
			default:
				// kill the worker from the channel
				wrk.State().Transition(fsm.StateInvalid)
				_ = wrk.Kill()

				continue
			}
			/*
				Bad states are here.
			*/
		default:
			// kill the current worker (just to be sure it's dead)
			if wrk != nil {
				_ = wrk.Kill()
			}
		}
	}
}
