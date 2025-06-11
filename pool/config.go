package pool

import (
	"runtime"
	"time"
)

// Config .. Pool config Configures the pool behavior.
type Config struct {
	// Debug flag creates new fresh worker before every request.
	Debug bool
	// Command used to override the server command with the custom one
	Command []string `mapstructure:"command"`
	// MaxQueueSize is maximum allowed queue size with the pending requests to the workers poll
	MaxQueueSize uint64 `mapstructure:"max_queue_size"`
	// NumWorkers defines how many sub-processes can be run at once. This value
	// might be doubled by Swapper while hot-swap. Defaults to number of CPU cores.
	NumWorkers uint64 `mapstructure:"num_workers"`
	// MaxJobs defines how many executions is allowed for the worker until
	// its destruction. set 1 to create new process for each new task, 0 to let
	// worker handle as many tasks as it can.
	MaxJobs uint64 `mapstructure:"max_jobs"`
	// AllocateTimeout defines for how long pool will be waiting for a worker to
	// be freed to handle the task. Defaults to 60s.
	AllocateTimeout time.Duration `mapstructure:"allocate_timeout"`
	// DestroyTimeout defines for how long pool should be waiting for worker to
	// properly destroy, if timeout reached worker will be killed. Defaults to 60s.
	DestroyTimeout time.Duration `mapstructure:"destroy_timeout"`
	// ResetTimeout defines how long pool should wait before start killing workers
	ResetTimeout time.Duration `mapstructure:"reset_timeout"`
	// Stream read operation timeout
	StreamTimeout time.Duration `mapstructure:"stream_timeout"`
	// Supervision config to limit worker and pool memory usage.
	Supervisor *SupervisorConfig `mapstructure:"supervisor"`
	// Dynamic allocation config
	DynamicAllocatorOpts *DynamicAllocationOpts `mapstructure:"dynamic_allocator"`
}

// InitDefaults enables default config values.
func (cfg *Config) InitDefaults() {
	if cfg.NumWorkers == 0 {
		cfg.NumWorkers = uint64(runtime.NumCPU()) //nolint:gosec
	}

	if cfg.AllocateTimeout == 0 {
		cfg.AllocateTimeout = time.Minute
	}

	if cfg.StreamTimeout == 0 {
		cfg.StreamTimeout = time.Minute
	}

	if cfg.DestroyTimeout == 0 {
		cfg.DestroyTimeout = time.Minute
	}

	if cfg.ResetTimeout == 0 {
		cfg.ResetTimeout = time.Minute
	}

	if cfg.Supervisor != nil {
		cfg.Supervisor.InitDefaults()
	}

	// initialize the dynamic allocator
	if cfg.DynamicAllocatorOpts != nil {
		cfg.DynamicAllocatorOpts.InitDefaults()
	}
}

type SupervisorConfig struct {
	// WatchTick defines how often to check the state of worker.
	WatchTick time.Duration `mapstructure:"watch_tick"`
	// TTL defines the maximum time for the worker is allowed to live.
	TTL time.Duration `mapstructure:"ttl"`
	// IdleTTL defines the maximum duration worker can spend in idle mode. Disabled when 0.
	IdleTTL time.Duration `mapstructure:"idle_ttl"`
	// ExecTTL defines maximum lifetime per job.
	ExecTTL time.Duration `mapstructure:"exec_ttl"`
	// MaxWorkerMemory limits memory per worker.
	MaxWorkerMemory uint64 `mapstructure:"max_worker_memory"`
}

// InitDefaults enables default config values.
func (cfg *SupervisorConfig) InitDefaults() {
	if cfg.WatchTick == 0 {
		cfg.WatchTick = time.Second * 5
	}
}

type DynamicAllocationOpts struct {
	MaxWorkers  uint64        `mapstructure:"max_workers"`
	SpawnRate   uint64        `mapstructure:"spawn_rate"`
	IdleTimeout time.Duration `mapstructure:"idle_timeout"`
	// Threshold defines at what percentage of remaining free workers the dynamic allocation mechanism should be triggered
	Threshold uint64 `mapstructure:"threshold"`
}

func (d *DynamicAllocationOpts) InitDefaults() {
	if d.MaxWorkers == 0 {
		d.MaxWorkers = 10
	}

	// limit max workers to 100
	if d.MaxWorkers > 100 {
		d.MaxWorkers = 100
	}

	if d.SpawnRate == 0 {
		d.SpawnRate = 5
	}

	if d.SpawnRate > 100 {
		d.SpawnRate = 100
	}

	if d.IdleTimeout == 0 || d.IdleTimeout < time.Second {
		d.IdleTimeout = time.Minute
	}

	// Set default threshold to 20% if not specified
	if d.Threshold == 0 {
		d.Threshold = 20
	}

	// Ensure threshold is between 0 and 100
	if d.Threshold > 100 {
		d.Threshold = 100
	}
}
