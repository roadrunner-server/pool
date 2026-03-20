package fsm

import (
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_NewState(t *testing.T) {
	log := slog.Default()
	st := NewFSM(StateErrored, log)

	assert.Equal(t, "errored", st.String())

	assert.Equal(t, "inactive", NewFSM(StateInactive, log).String())
	assert.Equal(t, "ready", NewFSM(StateReady, log).String())
	assert.Equal(t, "working", NewFSM(StateWorking, log).String())
	assert.Equal(t, "stopped", NewFSM(StateStopped, log).String())
	assert.Equal(t, "undefined", NewFSM(1000, log).String())
}

func Test_IsActive(t *testing.T) {
	log := slog.Default()
	assert.False(t, NewFSM(StateInactive, log).IsActive())
	assert.True(t, NewFSM(StateReady, log).IsActive())
	assert.True(t, NewFSM(StateWorking, log).IsActive())
	assert.False(t, NewFSM(StateStopped, log).IsActive())
	assert.False(t, NewFSM(StateErrored, log).IsActive())
}

// TestFSM_Recognizer_ValidTransitions verifies every legal transition path the pool uses succeeds.
// If a valid path is accidentally broken, workers freeze in their current state forever.
func TestFSM_Recognizer_ValidTransitions(t *testing.T) {
	log := slog.Default()

	tests := []struct {
		name string
		path []int64
	}{
		{
			name: "normal exec cycle: Inactive->Ready->Working->Ready",
			path: []int64{StateInactive, StateReady, StateWorking, StateReady},
		},
		{
			name: "supervisor TTL: Ready->StateTTLReached",
			path: []int64{StateReady, StateTTLReached},
		},
		{
			name: "supervisor memory: Ready->StateMaxMemoryReached",
			path: []int64{StateReady, StateMaxMemoryReached},
		},
		{
			name: "supervisor idle: Ready->StateIdleTTLReached",
			path: []int64{StateReady, StateIdleTTLReached},
		},
		{
			name: "supervisor marks busy worker: Working->StateInvalid",
			path: []int64{StateReady, StateWorking, StateInvalid},
		},
		{
			name: "max jobs reached: Ready->StateMaxJobsReached",
			path: []int64{StateReady, StateMaxJobsReached},
		},
		{
			name: "destroy from ready: Ready->StateDestroyed",
			path: []int64{StateReady, StateDestroyed},
		},
		{
			name: "destroy from working: Working->StateDestroyed",
			path: []int64{StateReady, StateWorking, StateDestroyed},
		},
		{
			name: "destroy from inactive: Inactive->StateDestroyed",
			path: []int64{StateInactive, StateDestroyed},
		},
		{
			name: "graceful stop: Working->StateStopping->StateStopped",
			path: []int64{StateReady, StateWorking, StateStopping, StateStopped},
		},
		{
			name: "exec TTL reached: Working->StateExecTTLReached",
			path: []int64{StateReady, StateWorking, StateExecTTLReached},
		},
		{
			name: "error from ready: Ready->StateErrored",
			path: []int64{StateReady, StateErrored},
		},
		{
			// Documents intentional behavior: any non-destroyed state can transition to Inactive.
			// The recognizer only blocks Destroyed->Inactive; everything else falls through.
			name: "Ready->Inactive (allow-all for Inactive)",
			path: []int64{StateReady, StateInactive},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.GreaterOrEqual(t, len(tt.path), 2, "path must have at least 2 states")
			f := NewFSM(tt.path[0], log)
			for i := 1; i < len(tt.path); i++ {
				f.Transition(tt.path[i])
				assert.Equal(t, tt.path[i], f.CurrentState(),
					"transition to %d failed from %d at step %d", tt.path[i], tt.path[i-1], i)
			}
		})
	}
}

// TestFSM_Recognizer_BlockedTransitions verifies illegal transitions are rejected.
// Documents the state machine contract — terminal states can't resurrect, self-transitions are rejected, and invalid source states are blocked.
func TestFSM_Recognizer_BlockedTransitions(t *testing.T) {
	log := slog.Default()

	tests := []struct {
		name string
		from int64
		to   int64
	}{
		{name: "Destroyed->Ready", from: StateDestroyed, to: StateReady},
		{name: "Destroyed->Working", from: StateDestroyed, to: StateWorking},
		{name: "Destroyed->Inactive", from: StateDestroyed, to: StateInactive},
		{name: "Destroyed->StateInvalid", from: StateDestroyed, to: StateInvalid},
		{name: "Destroyed->StateStopping", from: StateDestroyed, to: StateStopping},
		{name: "Ready->Ready", from: StateReady, to: StateReady},
		{name: "Working->Working (only from Ready)", from: StateWorking, to: StateWorking},
		{name: "Stopped->Ready (not in allowed from list)", from: StateStopped, to: StateReady},
		{name: "StateErrored->Ready", from: StateErrored, to: StateReady},
		{name: "StateErrored->Working", from: StateErrored, to: StateWorking},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := NewFSM(tt.from, log)
			f.Transition(tt.to)
			assert.Equal(t, tt.from, f.CurrentState(),
				"transition from %d to %d should have been blocked", tt.from, tt.to)
		})
	}
}

// TestFSM_ConcurrentTransitions verifies state consistency under concurrent transitions.
// There's a TOCTOU gap between recognizer reading currentState and Store —
// the supervisor, exec path, and watcher all call Transition concurrently.
func TestFSM_ConcurrentTransitions(t *testing.T) {
	log := slog.Default()
	const goroutines = 100

	// Phase 1: N goroutines all attempting Ready -> Working
	f := NewFSM(StateReady, log)
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			f.Transition(StateWorking)
		})
	}
	wg.Wait()
	assert.Equal(t, StateWorking, f.CurrentState())

	// Phase 2: N goroutines all attempting Working -> Ready
	for range goroutines {
		wg.Go(func() {
			f.Transition(StateReady)
		})
	}
	wg.Wait()
	assert.Equal(t, StateReady, f.CurrentState())

	// Phase 3: Rapid cycle Ready -> Working -> Ready under concurrency
	f2 := NewFSM(StateReady, log)
	for range goroutines {
		wg.Go(func() {
			f2.Transition(StateWorking)
			f2.Transition(StateReady)
		})
	}
	wg.Wait()
	// Final state must be one of the valid states
	state := f2.CurrentState()
	assert.True(t, state == StateReady || state == StateWorking,
		"final state should be Ready or Working, got %d", state)
}

// TestFSM_RegisterExec_Concurrent validates that NumExecs is accurate under concurrency.
// Load-bearing for MaxJobs enforcement — if undercounts, workers never get replaced.
func TestFSM_RegisterExec_Concurrent(t *testing.T) {
	log := slog.Default()
	f := NewFSM(StateReady, log)

	const goroutines = 100
	const execsPerGoroutine = 100

	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range execsPerGoroutine {
				f.RegisterExec()
			}
		})
	}
	wg.Wait()

	assert.Equal(t, uint64(goroutines*execsPerGoroutine), f.NumExecs())
}
