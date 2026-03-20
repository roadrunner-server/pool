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
	err := s.recognizer(to)
	if err != nil {
		s.log.Debug("transition info, this is not an error", "reason", err.Error())
		return
	}

	s.currentState.Store(to)
}

// String returns current StateImpl as string.
func (s *Fsm) String() string {
	switch s.currentState.Load() {
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
	default:
		return "undefined"
	}
}

// NumExecs returns number of registered WorkerProcess execs.
func (s *Fsm) NumExecs() uint64 {
	return s.numExecs.Load()
}

// IsActive returns true if WorkerProcess not Inactive or Stopped
func (s *Fsm) IsActive() bool {
	return s.currentState.Load() == StateWorking ||
		s.currentState.Load() == StateReady
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
func (s *Fsm) recognizer(to int64) error {
	const op = errors.Op("fsm_recognizer")
	switch to {
	// to
	case StateInactive:
		// from
		// No-one can transition to Inactive
		if s.currentState.Load() == StateDestroyed {
			return errors.E(op, errors.Errorf("can't transition from state: %s", s.String()))
		}
	// to from StateWorking/StateInactive only
	case StateReady:
		// from
		switch s.currentState.Load() {
		case StateWorking, StateInactive:
			return nil
		default:
			return errors.E(op, errors.Errorf("can't transition from state: %s", s.String()))
		}

	// to
	case StateWorking:
		// from
		// StateWorking can be transitioned only from StateReady
		if s.currentState.Load() == StateReady {
			return nil
		}

		return errors.E(op, errors.Errorf("can't transition from state: %s", s.String()))
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
		// from
		if s.currentState.Load() == StateDestroyed {
			return errors.E(op, errors.Errorf("can't transition from state: %s", s.String()))
		}
	// to
	case StateDestroyed:
		return nil
	}

	return nil
}
