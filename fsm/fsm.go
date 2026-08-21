package fsm

import (
	"log/slog"
	"sync/atomic"

	"github.com/roadrunner-server/errors"
)

// NewFSM returns new FSM implementation based on initial state
func NewFSM(initialState int64, log *slog.Logger) *Fsm {
	f := &Fsm{log: log}
	f.currentState.Store(initialState)
	return f
}

// Fsm is general https://en.wikipedia.org/wiki/Finite-state_machine to transition between worker states
type Fsm struct {
	log      *slog.Logger
	numExecs atomic.Uint64
	// to be lightweight, use UnixNano
	lastUsed     atomic.Uint64
	currentState atomic.Int64
}

// CurrentState (see interface)
func (s *Fsm) CurrentState() int64 {
	return s.currentState.Load()
}

func (s *Fsm) Compare(state int64) bool {
	return s.currentState.Load() == state
}

/*
Transition moves worker from one state to another
*/
func (s *Fsm) Transition(to int64) {
	// validate and store atomically
	for {
		from := s.currentState.Load()
		err := s.recognizer(from, to)
		if err != nil {
			s.log.Debug("transition info, this is not an error", "reason", err.Error())
			return
		}

		if s.currentState.CompareAndSwap(from, to) {
			return
		}
	}
}

func (s *Fsm) TransitionFrom(from, to int64) bool {
	if err := s.recognizer(from, to); err != nil {
		s.log.Debug("transition info, this is not an error", "reason", err.Error())
		return false
	}

	return s.currentState.CompareAndSwap(from, to)
}

// String returns current StateImpl as string.
func (s *Fsm) String() string {
	return stateName(s.currentState.Load())
}

func stateName(state int64) string {
	switch state {
	case StateInactive:
		return "inactive"
	case StateReady:
		return "ready"
	case StateWorking:
		return "working"
	case StateInvalid:
		return "invalid"
	case StateStopping:
		return "stopping"
	case StateStopped:
		return "stopped"
	case StateErrored:
		return "errored"
	case StateDestroyed:
		return "destroyed"
	case StateMaxJobsReached:
		return "maxJobsReached"
	case StateIdleTTLReached:
		return "idleTTLReached"
	case StateTTLReached:
		return "ttlReached"
	case StateMaxMemoryReached:
		return "maxMemoryReached"
	case StateExecTTLReached:
		return "execTTLReached"
	default:
		return "undefined"
	}
}

// NumExecs returns number of registered WorkerProcess execs.
func (s *Fsm) NumExecs() uint64 {
	return s.numExecs.Load()
}

// IsActive returns true if the worker is in the Ready or Working state.
func (s *Fsm) IsActive() bool {
	st := s.currentState.Load()
	return st == StateWorking || st == StateReady
}

// RegisterExec register new execution atomically
func (s *Fsm) RegisterExec() {
	s.numExecs.Add(1)
}

// SetLastUsed Update last used time
func (s *Fsm) SetLastUsed(lu uint64) {
	s.lastUsed.Store(lu)
}

func (s *Fsm) LastUsed() uint64 {
	return s.lastUsed.Load()
}

// Acceptors (also called detectors or recognizers) produce binary output,
// indicating whether or not the received input is accepted.
// Each event of an acceptor is either accepting or non accepting.
func (s *Fsm) recognizer(from, to int64) error {
	const op = errors.Op("fsm_recognizer")
	switch to {
	// to
	case StateInactive:
		// from: any state except Destroyed
		if from == StateDestroyed {
			return errors.E(op, errors.Errorf("can't transition from state: %s", stateName(from)))
		}
	// to
	case StateReady:
		// from: Working or Inactive only
		switch from {
		case StateWorking, StateInactive:
			return nil
		default:
			return errors.E(op, errors.Errorf("can't transition from state: %s", stateName(from)))
		}

	// to
	case StateWorking:
		// from: Ready only
		if from == StateReady {
			return nil
		}

		return errors.E(op, errors.Errorf("can't transition from state: %s", stateName(from)))
	// to
	case
		StateInvalid,
		StateStopping,
		StateStopped,
		StateMaxJobsReached,
		StateErrored,
		StateIdleTTLReached,
		StateTTLReached,
		StateMaxMemoryReached,
		StateExecTTLReached:
		// from: any state except Destroyed
		if from == StateDestroyed {
			return errors.E(op, errors.Errorf("can't transition from state: %s", stateName(from)))
		}
	// to
	case StateDestroyed:
		return nil
	}

	return nil
}
