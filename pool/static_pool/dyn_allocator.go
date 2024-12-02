package static_pool //nolint:stylecheck

import (
	"context"
	stderr "errors"
	"time"

	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/pool/fsm"
	"github.com/roadrunner-server/pool/worker"
	"go.uber.org/zap"
)

func (sp *Pool) allocateDynamically() (*worker.Process, error) {
	const op = errors.Op("allocate_dynamically")

	// obtain an operation lock
	// we can use a lock-free approach here, but it's not necessary
	sp.cfg.DynamicAllocatorOpts.Lock()
	defer sp.cfg.DynamicAllocatorOpts.Unlock()

	if !sp.cfg.DynamicAllocatorOpts.IsStarted() {
		// start the dynamic allocator listener
		sp.dynamicTTLListener()
	}

	// reset the TTL listener
	sp.cfg.DynamicAllocatorOpts.TriggerTTL()

	// if we already allocated max workers, we can't allocate more
	if sp.cfg.DynamicAllocatorOpts.CurrAllocated() >= sp.cfg.DynamicAllocatorOpts.MaxWorkers {
		// can't allocate more
		return nil, errors.E(op, stderr.New("can't allocate more workers, increase max_workers option"))
	}

	// if we have dynamic allocator, we can try to allocate new worker
	// this worker should not be released here, but instead will be released in the Exec function
	nw, err := sp.allocator()
	if err != nil {
		sp.log.Error("failed to allocate dynamic worker", zap.Error(err))
		return nil, errors.E(op, err)
	}

	watchW := make([]*worker.Process, 0, 10)
	watchW = append(watchW, nw)

	// increase number of additionally allocated options
	sp.cfg.DynamicAllocatorOpts.IncAllocated()

	sp.log.Debug("allocated additional worker",
		zap.Int64("pid", nw.Pid()),
		zap.Uint64("max_execs", nw.MaxExecs()),
		zap.Uint64("currently additionally allocated", sp.cfg.DynamicAllocatorOpts.CurrAllocated()),
	)

	// we starting from the 1 because we already allocated one worker which would be released in the Exec function
	for i := uint64(1); i <= sp.cfg.DynamicAllocatorOpts.SpawnRate; i++ {
		// spawn as much workers as user specified in the spawn rate configuration, but not more than max workers
		if sp.cfg.DynamicAllocatorOpts.CurrAllocated() >= sp.cfg.DynamicAllocatorOpts.MaxWorkers {
			break
		}

		bw, err := sp.allocator()
		if err != nil {
			return nil, errors.E(op, err)
		}

		sp.cfg.DynamicAllocatorOpts.IncAllocated()
		// add worker to the watcher
		watchW = append(watchW, bw)

		sp.log.Debug("allocated additional worker",
			zap.Int64("pid", bw.Pid()),
			zap.Uint64("max_execs", bw.MaxExecs()),
			zap.Uint64("currently additionally allocated", sp.cfg.DynamicAllocatorOpts.CurrAllocated()),
		)
	}

	err = sp.ww.Watch(watchW)
	if err != nil {
		return nil, errors.E(op, err)
	}

	return nw, nil
}

func (sp *Pool) dynamicTTLListener() {
	if sp.cfg.DynamicAllocatorOpts == nil || sp.cfg.Debug {
		return
	}

	sp.log.Debug("starting dynamic allocator listener", zap.Duration("idle_timeout", sp.cfg.DynamicAllocatorOpts.IdleTimeout))
	go func() {
		// DynamicAllocatorOpts are read-only, so we can use them without a lock
		triggerTTL := time.NewTicker(sp.cfg.DynamicAllocatorOpts.IdleTimeout)
		for {
			select {
			case <-sp.stopCh:
				sp.log.Debug("dynamic allocator listener stopped")
				goto exit
			// when this channel is triggered, we should deallocate all dynamically allocated workers
			case <-triggerTTL.C:
				sp.log.Debug("dynamic workers TTL", zap.String("reason", "idle timeout reached"))
				// get the Exec (the whole operation) lock
				sp.mu.Lock()
				// get the DynamicAllocatorOpts lock to prevent operations on the CurrAllocated
				sp.cfg.DynamicAllocatorOpts.Lock()

				// if we don't have any dynamically allocated workers, we can skip the deallocation
				if sp.cfg.DynamicAllocatorOpts.CurrAllocated() == 0 {
					sp.cfg.DynamicAllocatorOpts.Unlock()
					sp.mu.Unlock()
					goto exit
				}

				for i := sp.cfg.DynamicAllocatorOpts.CurrAllocated(); i > 0; i-- {
					// take the worker from the stack, inifinite timeout
					w, err := sp.ww.Take(context.Background())
					if err != nil {
						sp.log.Error("failed to take worker from the stack", zap.Error(err))
						continue
					}

					// set the worker state to be destroyed
					w.State().Transition(fsm.StateDestroyed)
					// release the worker
					sp.ww.Release(w)
				}

				sp.cfg.DynamicAllocatorOpts.ResetAllocated()

				sp.cfg.DynamicAllocatorOpts.Unlock()
				sp.mu.Unlock()
				sp.log.Debug("dynamic workers deallocated", zap.String("reason", "idle timeout reached"))
				triggerTTL.Stop()
				goto exit

				// when this channel is triggered, we should extend the TTL of all dynamically allocated workers
			case <-sp.cfg.DynamicAllocatorOpts.GetTriggerTTLChan():
				triggerTTL.Reset(sp.cfg.DynamicAllocatorOpts.IdleTimeout)
			}
		}
	exit:
		sp.cfg.DynamicAllocatorOpts.Stop()
	}()
}
