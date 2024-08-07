package fsm

import (
	"sync/atomic"

	"github.com/roadrunner-server/errors"
	"go.uber.org/zap"
)

// NewFSM returns new FSM implementation based on initial state
func NewFSM(initialState int64, log *zap.Logger) *Fsm {
	return &Fsm{
		log:          log,
		currentState: &initialState,
	}
}

// Fsm is general https://en.wikipedia.org/wiki/Finite-state_machine to transition between worker states
type Fsm struct {
	log      *zap.Logger
	numExecs uint64
	// to be lightweight, use UnixNano
	lastUsed     uint64
	currentState *int64
}

// CurrentState (see interface)
func (s *Fsm) CurrentState() int64 {
	return atomic.LoadInt64(s.currentState)
}

func (s *Fsm) Compare(state int64) bool {
	return atomic.LoadInt64(s.currentState) == state
}

/*
Transition moves worker from one state to another
*/
func (s *Fsm) Transition(to int64) {
	err := s.recognizer(to)
	if err != nil {
		s.log.Debug("transition info, this is not an error", zap.String("debug", err.Error()))
		return
	}

	atomic.StoreInt64(s.currentState, to)
}

// String returns current StateImpl as string.
func (s *Fsm) String() string {
	switch atomic.LoadInt64(s.currentState) {
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
	}

	return "undefined"
}

// NumExecs returns number of registered WorkerProcess execs.
func (s *Fsm) NumExecs() uint64 {
	return atomic.LoadUint64(&s.numExecs)
}

// IsActive returns true if WorkerProcess not Inactive or Stopped
func (s *Fsm) IsActive() bool {
	return atomic.LoadInt64(s.currentState) == StateWorking ||
		atomic.LoadInt64(s.currentState) == StateReady
}

// RegisterExec register new execution atomically
func (s *Fsm) RegisterExec() {
	atomic.AddUint64(&s.numExecs, 1)
}

// SetLastUsed Update last used time
func (s *Fsm) SetLastUsed(lu uint64) {
	atomic.StoreUint64(&s.lastUsed, lu)
}

func (s *Fsm) LastUsed() uint64 {
	return atomic.LoadUint64(&s.lastUsed)
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
		if atomic.LoadInt64(s.currentState) == StateDestroyed {
			return errors.E(op, errors.Errorf("can't transition from state: %s", s.String()))
		}
	// to from StateWorking/StateInactive only
	case StateReady:
		// from
		switch atomic.LoadInt64(s.currentState) {
		case StateWorking, StateInactive:
			return nil
		}

		return errors.E(op, errors.Errorf("can't transition from state: %s", s.String()))
	// to
	case StateWorking:
		// from
		// StateWorking can be transitioned only from StateReady
		if atomic.LoadInt64(s.currentState) == StateReady {
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
		if atomic.LoadInt64(s.currentState) == StateDestroyed {
			return errors.E(op, errors.Errorf("can't transition from state: %s", s.String()))
		}
	// to
	case StateDestroyed:
		return nil
	}

	return nil
}
