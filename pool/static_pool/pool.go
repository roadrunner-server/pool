package static_pool //nolint:stylecheck

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/events"
	"github.com/roadrunner-server/goridge/v3/pkg/frame"
	"github.com/roadrunner-server/pool/fsm"
	"github.com/roadrunner-server/pool/payload"
	"github.com/roadrunner-server/pool/pool"
	"github.com/roadrunner-server/pool/worker"
	workerWatcher "github.com/roadrunner-server/pool/worker_watcher"
	"go.uber.org/zap"
)

const (
	// StopRequest can be sent by worker to indicate that restart is required.
	StopRequest = `{"stop":true}`
)

// Pool controls worker creation, destruction and task routing. Pool uses fixed amount of stack.
type Pool struct {
	// pool configuration
	cfg *pool.Config
	// logger
	log *zap.Logger
	// worker command creator
	cmd pool.Command
	// creates and connects to stack
	factory pool.Factory
	// manages worker states and TTLs
	ww *workerWatcher.WorkerWatcher
	// dynamic allocator
	dynamicAllocator *dynAllocator
	// allocate new worker
	allocator func() (*worker.Process, error)
	// exec queue size
	queue        uint64
	maxQueueSize uint64
	// used in the supervised mode
	supervisedExec bool
	stopCh         chan struct{}
	mu             sync.RWMutex
}

// NewPool creates a new worker pool and task multiplexer. Pool will initiate with one worker. If supervisor configuration is provided -> pool will be turned into a supervisedExec mode
func NewPool(ctx context.Context, cmd pool.Command, factory pool.Factory, cfg *pool.Config, log *zap.Logger, options ...Options) (*Pool, error) {
	if factory == nil {
		return nil, errors.Str("no factory initialized")
	}

	if cfg == nil {
		return nil, errors.Str("nil configuration provided")
	}

	cfg.InitDefaults()

	// limit the number of workers to 500
	if cfg.NumWorkers > 500 {
		return nil, errors.Str("number of workers can't be more than 500")
	}

	// for debug mode we need to set the number of workers to 0 (no pre-allocated workers) and max jobs to 1
	if cfg.Debug {
		cfg.NumWorkers = 0
		cfg.MaxJobs = 1
		cfg.MaxQueueSize = 0
	}

	p := &Pool{
		cfg:     cfg,
		cmd:     cmd,
		factory: factory,
		log:     log,
		queue:   0,
		stopCh:  make(chan struct{}),
	}

	// apply options
	for i := range options {
		options[i](p)
	}

	if p.log == nil {
		var err error
		p.log, err = zap.NewDevelopment()
		if err != nil {
			return nil, err
		}
	}

	// set up workers allocator
	p.allocator = pool.NewPoolAllocator(ctx, p.cfg.AllocateTimeout, p.cfg.MaxJobs, factory, cmd, p.cfg.Command, p.log)
	// set up workers watcher
	p.ww = workerWatcher.NewSyncWorkerWatcher(p.allocator, p.log, p.cfg.NumWorkers, p.cfg.AllocateTimeout)

	// allocate requested number of workers
	workers, err := pool.AllocateParallel(p.cfg.NumWorkers, p.allocator)
	if err != nil {
		return nil, err
	}

	// add workers to the watcher
	err = p.ww.Watch(workers)
	if err != nil {
		return nil, err
	}

	if p.cfg.Supervisor != nil {
		if p.cfg.Supervisor.ExecTTL != 0 {
			// we use supervisedExec ExecWithTTL mode only when ExecTTL is set
			// otherwise we may use a faster Exec
			p.supervisedExec = true
		}
		// start the supervisor
		p.start()
	}

	if p.cfg.DynamicAllocatorOpts != nil {
		p.dynamicAllocator = newDynAllocator(p.log, p.ww, p.allocator, p.stopCh, &p.mu, p.cfg)
	}

	return p, nil
}

// GetConfig returns associated pool configuration. Immutable.
func (sp *Pool) GetConfig() *pool.Config {
	return sp.cfg
}

// Workers returns worker list associated with the pool.
func (sp *Pool) Workers() (workers []*worker.Process) {
	return sp.ww.List()
}

func (sp *Pool) RemoveWorker(ctx context.Context) error {
	if sp.cfg.Debug {
		sp.log.Warn("remove worker operation is not allowed in debug mode")
		return nil
	}
	var cancel context.CancelFunc
	_, ok := ctx.Deadline()
	if !ok {
		ctx, cancel = context.WithTimeout(ctx, sp.cfg.DestroyTimeout)
		defer cancel()
	}

	return sp.ww.RemoveWorker(ctx)
}

func (sp *Pool) AddWorker() error {
	if sp.cfg.Debug {
		sp.log.Warn("add worker operation is not allowed in debug mode")
		return nil
	}
	return sp.ww.AddWorker()
}

// Exec executes provided payload on the worker
func (sp *Pool) Exec(ctx context.Context, p *payload.Payload, stopCh chan struct{}) (chan *PExec, error) {
	const op = errors.Op("static_pool_exec")

	if len(p.Body) == 0 && len(p.Context) == 0 {
		return nil, errors.E(op, errors.Str("payload can not be empty"))
	}

	// check if we have space to put the request
	if atomic.LoadUint64(&sp.maxQueueSize) != 0 && atomic.LoadUint64(&sp.queue) >= atomic.LoadUint64(&sp.maxQueueSize) {
		return nil, errors.E(op, errors.QueueSize, errors.Str("max queue size reached"))
	}

	if sp.cfg.Debug {
		switch sp.supervisedExec {
		case true:
			ctxTTL, cancel := context.WithTimeout(ctx, sp.cfg.Supervisor.ExecTTL)
			defer cancel()
			return sp.execDebug(ctxTTL, p, stopCh)
		case false:
			return sp.execDebug(context.Background(), p, stopCh)
		}
	}

	/*
		register request in the QUEUE
	*/
	atomic.AddUint64(&sp.queue, 1)
	defer atomic.AddUint64(&sp.queue, ^uint64(0))

	// see notes at the end of the file
begin:
	ctxGetFree, cancel := context.WithTimeout(ctx, sp.cfg.AllocateTimeout)
	defer cancel()
	w, err := sp.takeWorker(ctxGetFree, op)
	if err != nil {
		return nil, errors.E(op, err)
	}

	var rsp *payload.Payload
	switch sp.supervisedExec {
	case true:
		// in the supervisedExec mode we're limiting the allowed time for the execution inside the PHP worker
		ctxT, cancelT := context.WithTimeout(ctx, sp.cfg.Supervisor.ExecTTL)
		defer cancelT()
		rsp, err = w.Exec(ctxT, p)
	case false:
		// no context here
		// potential problem: if the worker is hung, we can't stop it
		rsp, err = w.Exec(context.Background(), p)
	}

	if w.MaxExecsReached() {
		sp.log.Debug("requests execution limit reached, worker will be restarted", zap.Int64("pid", w.Pid()), zap.Uint64("execs", w.State().NumExecs()))
		w.State().Transition(fsm.StateMaxJobsReached)
	}

	if err != nil {
		// just push event if on any stage was timeout error
		switch {
		case errors.Is(errors.ExecTTL, err):
			// for this case, worker already killed in the ExecTTL function
			sp.log.Warn("worker stopped, and will be restarted", zap.String("reason", "execTTL timeout elapsed"), zap.Int64("pid", w.Pid()), zap.String("internal_event_name", events.EventExecTTL.String()), zap.Error(err))
			w.State().Transition(fsm.StateExecTTLReached)

			// worker should already be reallocated
			return nil, err
		case errors.Is(errors.SoftJob, err):
			/*
				in case of soft job error, we should not kill the worker, this is just an error payload from the worker.
			*/
			w.State().Transition(fsm.StateReady)
			sp.log.Warn("soft worker error", zap.String("reason", "SoftJob"), zap.Int64("pid", w.Pid()), zap.String("internal_event_name", events.EventWorkerSoftError.String()), zap.Error(err))
			sp.ww.Release(w)

			return nil, err
		case errors.Is(errors.Network, err):
			// in case of network error, we can't stop the worker, we should kill it
			w.State().Transition(fsm.StateErrored)
			sp.log.Warn("RoadRunner can't communicate with the worker", zap.String("reason", "worker hung or process was killed"), zap.Int64("pid", w.Pid()), zap.String("internal_event_name", events.EventWorkerError.String()), zap.Error(err))
			// kill the worker instead of sending a net packet to it
			_ = w.Kill()

			// do not return it, should be reallocated on Kill
			return nil, err
		case errors.Is(errors.Retry, err):
			// put the worker back to the stack and retry the request with the new one
			sp.ww.Release(w)
			goto begin

		default:
			w.State().Transition(fsm.StateErrored)
			sp.log.Warn("worker will be restarted", zap.Int64("pid", w.Pid()), zap.String("internal_event_name", events.EventWorkerDestruct.String()), zap.Error(err))

			sp.ww.Release(w)
			return nil, err
		}
	}

	// worker want's to be terminated
	// unsafe is used to quickly transform []byte to string
	if len(rsp.Body) == 0 && unsafe.String(unsafe.SliceData(rsp.Context), len(rsp.Context)) == StopRequest {
		w.State().Transition(fsm.StateInvalid)
		sp.ww.Release(w)
		goto begin
	}

	switch {
	case rsp.Flags&frame.STREAM != 0:
		sp.log.Debug("stream mode", zap.Int64("pid", w.Pid()))
		// create channel for the stream (only if there are no errors)
		// we need to create a buffered channel to prevent blocking
		// stream buffer size should be bigger than regular, to have some payloads ready (optimization)
		resp := make(chan *PExec, 5)
		// send the initial frame
		resp <- newPExec(rsp, nil)

		// in case of stream we should not return worker back immediately
		go func() {
			// would be called on Goexit
			defer func() {
				sp.log.Debug("release [stream] worker", zap.Int("pid", int(w.Pid())), zap.String("state", w.State().String()))
				close(resp)
				sp.ww.Release(w)
			}()

			// stream iterator
			for {
				select {
				// we received stop signal
				case <-stopCh:
					sp.log.Debug("stream stop signal received", zap.Int("pid", int(w.Pid())), zap.String("state", w.State().String()))
					ctxT, cancelT := context.WithTimeout(ctx, sp.cfg.StreamTimeout)
					err = w.StreamCancel(ctxT)
					cancelT()
					if err != nil {
						w.State().Transition(fsm.StateErrored)
						sp.log.Warn("stream cancel error", zap.Error(err))
					} else {
						// successfully canceled
						w.State().Transition(fsm.StateReady)
						sp.log.Debug("transition to the ready state", zap.String("from", w.State().String()))
					}

					runtime.Goexit()
				default:
					// we have to set a stream timeout on every request
					switch sp.supervisedExec {
					case true:
						ctxT, cancelT := context.WithTimeout(context.Background(), sp.cfg.Supervisor.ExecTTL)
						pld, next, errI := w.StreamIterWithContext(ctxT)
						cancelT()
						if errI != nil {
							sp.log.Warn("stream error", zap.Error(err))

							resp <- newPExec(nil, errI)

							// move worker to the invalid state to restart
							w.State().Transition(fsm.StateInvalid)
							runtime.Goexit()
						}

						resp <- newPExec(pld, nil)

						if !next {
							w.State().Transition(fsm.StateReady)
							// we've got the last frame
							runtime.Goexit()
						}
					case false:
						// non supervised execution, can potentially hang here
						pld, next, errI := w.StreamIter()
						if errI != nil {
							sp.log.Warn("stream iter error", zap.Error(err))
							// send error response
							resp <- newPExec(nil, errI)

							// move worker to the invalid state to restart
							w.State().Transition(fsm.StateInvalid)
							runtime.Goexit()
						}

						resp <- newPExec(pld, nil)

						if !next {
							w.State().Transition(fsm.StateReady)
							// we've got the last frame
							runtime.Goexit()
						}
					}
				}
			}
		}()

		return resp, nil
	default:
		resp := make(chan *PExec, 1)
		// send the initial frame
		resp <- newPExec(rsp, nil)
		sp.log.Debug("req-resp mode", zap.Int64("pid", w.Pid()))
		if w.State().Compare(fsm.StateWorking) {
			w.State().Transition(fsm.StateReady)
		}
		// return worker back
		sp.ww.Release(w)
		// close the channel
		close(resp)
		return resp, nil
	}
}

func (sp *Pool) QueueSize() uint64 {
	return atomic.LoadUint64(&sp.queue)
}

func (sp *Pool) NumDynamic() uint64 {
	if sp.cfg.DynamicAllocatorOpts == nil {
		return 0
	}

	return *sp.dynamicAllocator.currAllocated.Load()
}

// Destroy all underlying stack (but let them complete the task).
func (sp *Pool) Destroy(ctx context.Context) {
	sp.log.Info("destroy signal received", zap.Duration("timeout", sp.cfg.DestroyTimeout))
	var cancel context.CancelFunc
	_, ok := ctx.Deadline()
	if !ok {
		ctx, cancel = context.WithTimeout(ctx, sp.cfg.DestroyTimeout)
		defer cancel()
	}
	sp.ww.Destroy(ctx)
	atomic.StoreUint64(&sp.queue, 0)
	close(sp.stopCh)
}

func (sp *Pool) Reset(ctx context.Context) error {
	// set timeout
	ctx, cancel := context.WithTimeout(ctx, sp.cfg.ResetTimeout)
	defer cancel()
	// reset all workers
	numToAllocate := sp.ww.Reset(ctx)
	// re-allocate all workers
	workers, err := pool.AllocateParallel(numToAllocate, sp.allocator)
	if err != nil {
		return err
	}
	// add the NEW workers to the watcher
	err = sp.ww.Watch(workers)
	if err != nil {
		return err
	}

	return nil
}

func (sp *Pool) takeWorker(ctxGetFree context.Context, op errors.Op) (*worker.Process, error) {
	// Get function consumes context with timeout
	w, err := sp.ww.Take(ctxGetFree)
	if err != nil {
		// if the error is of kind NoFreeWorkers, it means, that we can't get worker from the stack during the allocate timeout
		if errors.Is(errors.NoFreeWorkers, err) {
			sp.log.Error(
				"no free workers in the pool, wait timeout exceed",
				zap.String("reason", "no free workers"),
				zap.String("internal_event_name", events.EventNoFreeWorkers.String()),
				zap.Error(err),
			)

			// if we don't have dynamic allocator or in debug mode, we can't allocate a new worker
			if sp.cfg.DynamicAllocatorOpts == nil || sp.cfg.Debug {
				return nil, errors.E(op, errors.NoFreeWorkers)
			}

			// for the dynamic allocator, we can would have many requests waiting at the same time on the lock in the dyn allocator
			// this will lead to the following case - all previous requests would be able to get the worker, since we're allocating them in the allocateDynamically
			// however, requests waiting for the lock, won't allocate a new worker and would be failed

			return sp.dynamicAllocator.allocateDynamically()
		}
		// else if err not nil - return error
		return nil, errors.E(op, err)
	}

	// Check if we need to allocate more workers based on the threshold
	if sp.cfg.DynamicAllocatorOpts != nil && !sp.cfg.Debug {
		// Get the total number of workers
		workers := sp.ww.List()
		totalWorkers := uint64(len(workers))
		freeWorkers := uint64(0)

		for _, w := range workers {
			if w.State().Compare(fsm.StateReady) {
				freeWorkers++
			}
		}

		if totalWorkers > 0 {
			// Calculate the percentage of free workers
			freeWorkersPercent := uint64(freeWorkers * 100 / totalWorkers)

			// If the percentage of free workers is below the threshold, allocate more workers
			if freeWorkersPercent <= sp.cfg.DynamicAllocatorOpts.Threshold {
				sp.log.Debug(
					"free workers percentage below threshold, allocating more workers",
					zap.Uint64("free_workers", freeWorkers),
					zap.Uint64("total_workers", totalWorkers),
					zap.Uint64("free_workers_percent", freeWorkersPercent),
					zap.Uint64("threshold", sp.cfg.DynamicAllocatorOpts.Threshold),
				)

				// Allocate more workers in a goroutine to avoid blocking the current request
				go func() {
					_, _ = sp.dynamicAllocator.allocateDynamically()
				}()
			}
		}
	}

	return w, nil
}
