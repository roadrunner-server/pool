package static_pool

import (
	"cmp"
	"context"
	"sync"
	"sync/atomic"
	"time"

	"log/slog"

	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/events"
	"github.com/roadrunner-server/goridge/v4/pkg/frame"
	"github.com/roadrunner-server/pool/v2/fsm"
	"github.com/roadrunner-server/pool/v2/payload"
	"github.com/roadrunner-server/pool/v2/pool"
	"github.com/roadrunner-server/pool/v2/worker"
	workerWatcher "github.com/roadrunner-server/pool/v2/worker_watcher"
)

const (
	// StopRequest can be sent by a worker to indicate that restart is required.
	StopRequest = `{"stop":true}`
)

// Pool controls worker creation, destruction and task routing. Pool uses a fixed number of workers.
type Pool struct {
	// pool configuration
	cfg *pool.Config
	// logger
	log *slog.Logger
	// manages worker states and TTLs
	ww *workerWatcher.WorkerWatcher
	// dynamic allocator
	dynamicAllocator *dynAllocator
	// allocate new worker
	allocator func() (*worker.Process, error)
	// exec queue size
	queue        atomic.Uint64
	maxQueueSize atomic.Uint64
	// used in the supervised mode
	supervisedExec bool
	// closed on Destroy; stops the supervisor and the dynamic allocator
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewPool creates a new worker pool and task multiplexer. Pool will initialize with the configured number of workers. If supervisor configuration is provided -> pool will be turned into a supervisedExec mode
func NewPool(ctx context.Context, cmd pool.Command, factory pool.Factory, cfg *pool.Config, log *slog.Logger, options ...Options) (*Pool, error) {
	if factory == nil {
		return nil, errors.Str("no factory initialized")
	}

	if cfg == nil {
		return nil, errors.Str("nil configuration provided")
	}

	cfg.InitDefaults()

	p := &Pool{
		cfg:    cfg,
		log:    log,
		stopCh: make(chan struct{}),
	}

	// options may adjust the config (e.g. WithNumWorkers), so validation and derived
	// defaults run again below
	for i := range options {
		options[i](p)
	}

	p.log = cmp.Or(p.log, slog.Default())

	// limit the number of workers to 500
	if cfg.NumWorkers > 500 {
		return nil, errors.Str("number of workers can't be more than 500")
	}

	// for debug mode we need to set the number of workers to 0 (no pre-allocated workers) and max jobs to 1
	if cfg.Debug {
		cfg.NumWorkers = 0
		cfg.MaxJobs = 1
		cfg.MaxQueueSize = 0
		p.maxQueueSize.Store(0)
	}

	// the dynamic allocator sizing depends on the worker count, which the options above may
	// have changed
	if cfg.DynamicAllocatorOpts != nil {
		cfg.DynamicAllocatorOpts.InitDefaults(cfg.NumWorkers)
	}

	// long-lived allocator for respawns and dynamic scale-up: detached from the constructor
	// context (a canceled parent must not break later allocations), but canceled on Destroy
	// so an in-flight spawn cannot stall the shutdown
	allocCtx, allocCancel := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		<-p.stopCh
		allocCancel()
	}()
	p.allocator = pool.NewPoolAllocator(allocCtx, p.cfg.AllocateTimeout, p.cfg.MaxJobs, factory, cmd, p.cfg.Command, p.log)
	// set up workers' watcher
	p.ww = workerWatcher.NewSyncWorkerWatcher(p.allocator, p.log, p.cfg.NumWorkers, p.cfg.AllocateTimeout)

	// the initial allocation stays cancelable through the caller's context
	initAllocator := pool.NewPoolAllocator(ctx, p.cfg.AllocateTimeout, p.cfg.MaxJobs, factory, cmd, p.cfg.Command, p.log)
	// allocate the requested number of workers
	workers, err := pool.AllocateParallel(p.cfg.NumWorkers, initAllocator)
	if err != nil {
		return nil, err
	}

	// add workers to the watcher
	p.ww.Watch(workers)

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
		p.dynamicAllocator = newDynAllocator(p.log, p.ww, p.stopCh, p.cfg)
	}

	return p, nil
}

// GetConfig returns the associated pool configuration. The pool owns it; callers must treat
// it as read-only.
func (sp *Pool) GetConfig() *pool.Config {
	return sp.cfg
}

// Workers returns a worker list associated with the pool.
func (sp *Pool) Workers() []*worker.Process {
	return sp.ww.List()
}

func (sp *Pool) RemoveWorker(ctx context.Context) error {
	if sp.cfg.Debug {
		sp.log.Warn("remove worker operation is not allowed in debug mode")
		return nil
	}
	ctx, cancel := ensureDeadline(ctx, sp.cfg.DestroyTimeout)
	defer cancel()

	return sp.ww.RemoveWorker(ctx)
}

// ensureDeadline bounds the context with the fallback timeout when the caller set no deadline.
func ensureDeadline(ctx context.Context, fallback time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}

	return context.WithTimeout(ctx, fallback)
}

// AddWorker adds one worker to the pool. With a dynamic allocator configured, workers above
// the base pool size are treated as dynamic and are subject to idle deallocation.
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
	if maxQueue := sp.maxQueueSize.Load(); maxQueue != 0 && sp.queue.Load() >= maxQueue {
		return nil, errors.E(op, errors.QueueSize, errors.Str("max queue size reached"))
	}

	if sp.cfg.Debug {
		return sp.execDebug(ctx, p, stopCh)
	}

	/*
		register a request in the QUEUE
	*/
	sp.queue.Add(1)
	defer sp.queue.Add(^uint64(0))

begin:
	w, err := sp.takeWorker(ctx, op)
	if err != nil {
		return nil, err
	}

	var rsp *payload.Payload
	if sp.supervisedExec {
		// in the supervisedExec mode we're limiting the allowed time for the execution inside the PHP worker
		ctxT, cancelT := context.WithTimeout(ctx, sp.cfg.Supervisor.ExecTTL)
		rsp, err = w.Exec(ctxT, p)
		cancelT()
	} else {
		// no context here
		// potential problem: if the worker is hung, we can't stop it
		rsp, err = w.Exec(context.Background(), p)
	}

	if w.MaxExecsReached() {
		sp.log.Debug("requests execution limit reached, worker will be restarted", "pid", w.Pid(), "execs", w.State().NumExecs())
		w.State().Transition(fsm.StateMaxJobsReached)
	}

	if err != nil {
		// just push event if on any stage was timeout error
		switch {
		case errors.Is(errors.ExecTTL, err):
			// in this case, the worker already killed in the ExecTTL function
			sp.log.Warn("worker stopped, and will be restarted", "reason", "execTTL timeout elapsed", "pid", w.Pid(), "internal_event_name", events.EventExecTTL.String(), "error", err)
			w.State().Transition(fsm.StateExecTTLReached)

			// worker should already be reallocated
			return nil, err
		case errors.Is(errors.SoftJob, err):
			/*
				in case of soft job error, we should not kill the worker; this is just an error payload from the worker.
			*/
			w.State().Transition(fsm.StateReady)
			sp.log.Warn("soft worker error", "reason", "SoftJob", "pid", w.Pid(), "internal_event_name", events.EventWorkerSoftError.String(), "error", err)
			sp.ww.Release(w)

			return nil, err
		case errors.Is(errors.Network, err):
			// in case of network error, we can't stop the worker, we should kill it
			w.State().Transition(fsm.StateErrored)
			sp.log.Warn("RoadRunner can't communicate with the worker", "reason", "worker hung or process was killed", "pid", w.Pid(), "internal_event_name", events.EventWorkerError.String(), "error", err)
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
			sp.log.Warn("worker will be restarted", "pid", w.Pid(), "internal_event_name", events.EventWorkerDestruct.String(), "error", err)

			sp.ww.Release(w)
			return nil, err
		}
	}

	// worker wants to be terminated
	if len(rsp.Body) == 0 && string(rsp.Context) == StopRequest {
		w.State().Transition(fsm.StateInvalid)
		sp.ww.Release(w)
		goto begin
	}

	switch {
	case rsp.Flags&frame.STREAM != 0:
		sp.log.Debug("stream mode", "pid", w.Pid())
		// buffered so a few frames are ready ahead of the consumer
		resp := make(chan *PExec, 5)
		// send the initial frame
		resp <- newPExec(rsp, nil)

		// in case of stream, the worker is released when the stream finishes
		go func() {
			defer func() {
				sp.log.Debug("release [stream] worker", "pid", w.Pid(), "state", w.State().String())
				close(resp)
				sp.ww.Release(w)
			}()

			sp.streamIterate(ctx, w, resp, stopCh)
		}()

		return resp, nil
	default:
		resp := make(chan *PExec, 1)
		// send the initial frame
		resp <- newPExec(rsp, nil)
		sp.log.Debug("req-resp mode", "pid", w.Pid())
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
	return sp.queue.Load()
}

// NumDynamic returns the number of workers currently held above the base pool size.
func (sp *Pool) NumDynamic() uint64 {
	if sp.cfg.DynamicAllocatorOpts == nil {
		return 0
	}

	return sp.dynamicAllocator.dynWorkers()
}

// Destroy all underlying workers (but let them complete the task).
func (sp *Pool) Destroy(ctx context.Context) {
	sp.log.Info("destroy signal received", "timeout", sp.cfg.DestroyTimeout)
	ctx, cancel := ensureDeadline(ctx, sp.cfg.DestroyTimeout)
	defer cancel()
	// stop the supervisor and the dynamic allocator first so nothing spawns workers while
	// the watcher shuts down; safe on repeated Destroy calls
	sp.stopOnce.Do(func() {
		close(sp.stopCh)
	})
	sp.ww.Destroy(ctx)
	sp.queue.Store(0)
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
		// the worker count is left untouched, so a later Reset can still re-allocate the
		// full pool once the allocation failure clears
		return err
	}
	// add the NEW workers to the watcher
	sp.ww.Watch(workers)

	return nil
}

func (sp *Pool) takeWorker(ctx context.Context, op errors.Op) (*worker.Process, error) {
	ctxGetFree, cancel := context.WithTimeout(ctx, sp.cfg.AllocateTimeout)
	defer cancel()

	w, err := sp.ww.Take(ctxGetFree)
	if err == nil {
		return w, nil
	}

	if !errors.Is(errors.NoFreeWorkers, err) {
		return nil, errors.E(op, err)
	}

	sp.log.Error(
		"no free workers in the pool, wait timeout exceed",
		"reason", "no free workers",
		"internal_event_name", events.EventNoFreeWorkers.String(),
		"error", err,
	)

	// without a dynamic allocator there is no way to get more workers
	if sp.cfg.DynamicAllocatorOpts == nil {
		return nil, errors.E(op, errors.NoFreeWorkers)
	}

	// the batch is already in the container when addMoreWorkers returns; a rate-limited call
	// means another caller's batch is in flight or just landed, so retrying is still useful
	res := sp.dynamicAllocator.addMoreWorkers()
	if res == nil {
		return nil, errors.E(op, errors.NoFreeWorkers)
	}

	sp.log.Debug("retrying the take after a scale-up attempt", "added", res.added, "rate_limited", res.rateLimited)
	ctxRetake, cancelRetake := context.WithTimeout(ctx, sp.cfg.AllocateTimeout)
	defer cancelRetake()

	w, err = sp.ww.Take(ctxRetake)
	if err != nil {
		return nil, errors.E(op, err)
	}

	return w, nil
}
