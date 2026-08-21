package static_pool

import (
	"time"

	"github.com/roadrunner-server/events"
	"github.com/roadrunner-server/pool/v2/fsm"
	"github.com/roadrunner-server/pool/v2/state/process"
)

const (
	MB = 1024 * 1024
)

func (sp *Pool) start() {
	go func() {
		watchTout := time.Tick(sp.cfg.Supervisor.WatchTick)

		for {
			select {
			case <-sp.stopCh:
				return
			// stop here
			case <-watchTout:
				sp.control()
			}
		}
	}()
}

func (sp *Pool) control() {
	now := time.Now()

	// a snapshot copy of the worker pointers
	workers := sp.Workers()

	for i := range workers {
		// if worker not in the Ready OR working state,
		// skip such a worker
		switch workers[i].State().CurrentState() {
		case
			fsm.StateInactive,
			fsm.StateErrored,
			fsm.StateStopping,
			fsm.StateStopped,
			fsm.StateInvalid,
			fsm.StateMaxJobsReached:

			// do not touch the bad worker until it pushed back to the stack
			continue

		case
			fsm.StateMaxMemoryReached,
			fsm.StateIdleTTLReached,
			fsm.StateTTLReached:
			// we can stop workers which reached the idlettl state
			// workers can be moved from these states ONLY by the supervisor and ONLY if the worker is in the StateReady
			if workers[i] != nil {
				_ = workers[i].Stop()
			}

			// call cleanup callback
			workers[i].Callback()

			continue
		default:
		}

		s, err := process.WorkerProcessState(workers[i])
		if err != nil {
			// worker not longer valid for supervision
			continue
		}

		if sp.cfg.Supervisor.TTL != 0 && now.Sub(workers[i].Created()) >= sp.cfg.Supervisor.TTL {
			if !workers[i].State().TransitionFrom(fsm.StateReady, fsm.StateTTLReached) {
				workers[i].State().Transition(fsm.StateInvalid)
			}

			sp.log.Debug("ttl", "reason", "ttl is reached", "pid", workers[i].Pid(), "internal_event_name", events.EventTTL.String())
			continue
		}

		if sp.cfg.Supervisor.MaxWorkerMemory != 0 && s.MemoryUsage >= sp.cfg.Supervisor.MaxWorkerMemory*MB {
			if !workers[i].State().TransitionFrom(fsm.StateReady, fsm.StateMaxMemoryReached) {
				workers[i].State().Transition(fsm.StateInvalid)
			}

			sp.log.Debug("memory_limit", "reason", "max memory is reached", "pid", workers[i].Pid(), "internal_event_name", events.EventMaxMemory.String())
			continue
		}

		// idle check: only ready workers accumulate idle time
		if sp.cfg.Supervisor.IdleTTL != 0 {
			if !workers[i].State().Compare(fsm.StateReady) {
				continue
			}

			// last used unix nano; zero means the worker was never used
			lu := workers[i].State().LastUsed()
			if lu == 0 {
				continue
			}
			if now.Sub(time.Unix(0, int64(lu))) >= sp.cfg.Supervisor.IdleTTL { //nolint:gosec
				if workers[i].State().TransitionFrom(fsm.StateReady, fsm.StateIdleTTLReached) {
					sp.log.Debug("idle_ttl", "reason", "idle ttl is reached", "pid", workers[i].Pid(), "internal_event_name", events.EventTTL.String())
				}
			}
		}
	}
}
