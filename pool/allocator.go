package pool

import (
	"context"
	"log/slog"

	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/events"
	"github.com/roadrunner-server/pool/v2/fsm"
	"github.com/roadrunner-server/pool/v2/worker"
	"golang.org/x/sync/errgroup"
)

// NewPoolAllocator initializes allocator of the workers: plain process spawn, no IPC handshake.
func NewPoolAllocator(ctx context.Context, cmd Command, command []string, log *slog.Logger) func() (*worker.Process, error) {
	return func() (*worker.Process, error) {
		// pool is being shut down / boot context canceled
		if err := ctx.Err(); err != nil {
			return nil, errors.E(errors.TimeOut, err)
		}

		w, err := worker.InitBaseWorker(cmd(command), worker.WithLog(log))
		if err != nil {
			return nil, err
		}

		err = w.Start()
		if err != nil {
			return nil, err
		}

		// readiness == process started; protocol-level readiness is plugin-side (the worker dials in)
		w.State().Transition(fsm.StateReady)

		log.Debug("worker is allocated", "pid", w.Pid(), "internal_event_name", events.EventWorkerConstruct.String())
		return w, nil
	}
}

// AllocateParallel allocate required number of stack
func AllocateParallel(numWorkers uint64, allocator func() (*worker.Process, error)) ([]*worker.Process, error) {
	const op = errors.Op("static_pool_allocate_workers")

	workers := make([]*worker.Process, numWorkers)
	eg := new(errgroup.Group)

	// constant number of stack simplify logic
	for i := range numWorkers {
		eg.Go(func() error {
			w, err := allocator()
			if err != nil {
				return errors.E(op, errors.WorkerAllocate, err)
			}

			workers[i] = w
			return nil
		})
	}

	err := eg.Wait()
	if err != nil {
		for j := range workers {
			if workers[j] != nil {
				go func() {
					_ = workers[j].Wait()
				}()

				_ = workers[j].Kill()
			}
		}
		return nil, err
	}

	return workers, nil
}
