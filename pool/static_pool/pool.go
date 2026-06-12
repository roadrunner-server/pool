package static_pool

import (
	"context"
	"log/slog"
	"sync"

	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/pool/v2/pool"
	"github.com/roadrunner-server/pool/v2/worker"
	workerWatcher "github.com/roadrunner-server/pool/v2/worker_watcher"
)

// Pool controls worker creation, destruction and supervision. Pool uses a fixed number of workers.
// The pool is a lifecycle manager only: request delivery to the workers is handled
// by the plugins (over connectrpc), not by the pool.
type Pool struct {
	// pool configuration
	cfg *pool.Config
	// logger
	log *slog.Logger
	// worker command creator
	cmd pool.Command
	// manages worker states and TTLs
	ww *workerWatcher.WorkerWatcher
	// dynamic allocator
	dynamicAllocator *dynAllocator
	// allocate new worker
	allocator func() (*worker.Process, error)
	stopCh    chan struct{}
	mu        sync.RWMutex
}

// NewPool creates a new worker pool. Pool will initialize with the configured number of workers.
// If supervisor configuration is provided -> pool will be supervised (TTL, idle TTL and memory checks).
func NewPool(ctx context.Context, cmd pool.Command, cfg *pool.Config, log *slog.Logger, options ...Options) (*Pool, error) {
	if cfg == nil {
		return nil, errors.Str("nil configuration provided")
	}

	cfg.InitDefaults()

	// limit the number of workers to 500
	if cfg.NumWorkers > 500 {
		return nil, errors.Str("number of workers can't be more than 500")
	}

	p := &Pool{
		cfg:    cfg,
		cmd:    cmd,
		log:    log,
		stopCh: make(chan struct{}),
	}

	// apply options
	for i := range options {
		options[i](p)
	}

	if p.log == nil {
		p.log = slog.Default()
	}

	// set up workers' allocator
	p.allocator = pool.NewPoolAllocator(ctx, cmd, p.cfg.Command, p.log)
	// set up workers' watcher
	p.ww = workerWatcher.NewSyncWorkerWatcher(p.allocator, p.log, p.cfg.NumWorkers, p.cfg.AllocateTimeout)

	// allocate the requested number of workers
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
		// start the supervisor
		p.start()
	}

	if p.cfg.DynamicAllocatorOpts != nil {
		p.dynamicAllocator = newDynAllocator(p.log, p.ww, p.allocator, p.stopCh, p.cfg)
	}

	return p, nil
}

// GetConfig returns associated pool configuration. Immutable.
func (sp *Pool) GetConfig() *pool.Config {
	return sp.cfg
}

// Workers returns a worker list associated with the pool.
func (sp *Pool) Workers() (workers []*worker.Process) {
	return sp.ww.List()
}

func (sp *Pool) RemoveWorker(ctx context.Context) error {
	var cancel context.CancelFunc
	_, ok := ctx.Deadline()
	if !ok {
		ctx, cancel = context.WithTimeout(ctx, sp.cfg.DestroyTimeout)
		defer cancel()
	}

	return sp.ww.RemoveWorker(ctx)
}

func (sp *Pool) AddWorker() error {
	return sp.ww.AddWorker()
}

func (sp *Pool) NumDynamic() uint64 {
	if sp.cfg.DynamicAllocatorOpts == nil {
		return 0
	}

	return sp.dynamicAllocator.currAllocated.Load()
}

// Destroy all underlying workers (but let them complete the task).
func (sp *Pool) Destroy(ctx context.Context) {
	sp.log.Info("destroy signal received", "timeout", sp.cfg.DestroyTimeout)
	var cancel context.CancelFunc
	_, ok := ctx.Deadline()
	if !ok {
		ctx, cancel = context.WithTimeout(ctx, sp.cfg.DestroyTimeout)
		defer cancel()
	}
	sp.ww.Destroy(ctx)
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
