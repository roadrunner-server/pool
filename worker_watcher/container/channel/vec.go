package channel

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/pool/v2/fsm"
	"github.com/roadrunner-server/pool/v2/worker"
)

type Vec struct {
	rwm sync.RWMutex
	// destroy signal
	destroy atomic.Bool
	// reset signal
	reset atomic.Bool
	// channel with the workers
	workers chan *worker.Process
}

// MaxWorkers caps the number of workers a single pool can hold; it is also the capacity of
// the container channel.
const MaxWorkers = 2048

func NewVector() *Vec {
	vec := &Vec{
		workers: make(chan *worker.Process, MaxWorkers),
	}

	return vec
}

// Push returns the worker to the container; when the container is full the worker is killed.
func (v *Vec) Push(w *worker.Process) {
	select {
	case v.workers <- w:
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
	if v.destroy.Load() {
		return nil, errors.E(errors.WatcherStopped)
	}

	// wait for the reset to complete
	for v.reset.Load() {
		select {
		case <-ctx.Done():
			return nil, errors.E(ctx.Err(), errors.NoFreeWorkers)
		default:
			time.Sleep(time.Millisecond * 10)
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
	v.reset.Store(false)
}

func (v *Vec) Reset() {
	v.reset.Store(true)
}

func (v *Vec) Destroy() {
	v.destroy.Store(true)
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

	// drain the vector, keeping healthy workers and killing the rest; while draining, a
	// reallocated worker might be pushed concurrently, so the push back may find the
	// channel full
	for range len(v.workers) {
		wrk := <-v.workers

		switch wrk.State().CurrentState() {
		// good states
		case fsm.StateWorking, fsm.StateReady:
			select {
			case v.workers <- wrk:
				continue
			default:
				// the channel is full; kill the worker
				wrk.State().Transition(fsm.StateInvalid)
				_ = wrk.Kill()

				continue
			}
		default:
			// bad state; make sure the worker is dead
			_ = wrk.Kill()
		}
	}
}
