package channel

import (
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/pool/v2/fsm"
	"github.com/roadrunner-server/pool/v2/worker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helper to create a simple worker for testing
func createTestWorker(t *testing.T) *worker.Process {
	t.Helper()
	cmd := exec.Command("php", "-v")
	w, err := worker.InitBaseWorker(cmd)
	require.NoError(t, err)
	return w
}

// createStartedTestWorker creates a worker with a running process (so Kill() won't panic on nil Process)
func createStartedTestWorker(t *testing.T) *worker.Process {
	t.Helper()
	cmd := exec.Command("sleep", "100")
	w, err := worker.InitBaseWorker(cmd)
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return w
}

// createTestWorkerInState creates a started worker in a specific FSM state.
// Workers must be started so that Kill() doesn't panic on nil Process.
func createTestWorkerInState(t *testing.T, state int64) *worker.Process {
	t.Helper()
	w := createStartedTestWorker(t)
	// Workers start in StateInactive; transition through valid paths
	switch state {
	case fsm.StateReady:
		w.State().Transition(fsm.StateReady)
	case fsm.StateWorking:
		w.State().Transition(fsm.StateReady)
		w.State().Transition(fsm.StateWorking)
	case fsm.StateErrored:
		w.State().Transition(fsm.StateErrored)
	case fsm.StateTTLReached:
		w.State().Transition(fsm.StateReady)
		w.State().Transition(fsm.StateTTLReached)
	case fsm.StateInvalid:
		w.State().Transition(fsm.StateReady)
		w.State().Transition(fsm.StateInvalid)
	}
	return w
}

// ==================== General Tests ====================

// Test_Vec_PushPop tests basic push and pop operations
func Test_Vec_PushPop(t *testing.T) {
	vec := NewVector()
	w := createTestWorker(t)

	// Push worker
	vec.Push(w)
	assert.Equal(t, 1, vec.Len())

	// Pop worker
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	popped, err := vec.Pop(ctx)
	require.NoError(t, err)
	assert.Equal(t, w, popped)
	assert.Equal(t, 0, vec.Len())
}

// Test_Vec_Len tests that Len returns correct count
func Test_Vec_Len(t *testing.T) {
	vec := NewVector()

	assert.Equal(t, 0, vec.Len())

	// Push multiple workers
	for range 5 {
		w := createTestWorker(t)
		vec.Push(w)
	}

	assert.Equal(t, 5, vec.Len())

	// Pop one
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	_, err := vec.Pop(ctx)
	require.NoError(t, err)

	assert.Equal(t, 4, vec.Len())
}

// ==================== Edge Case Tests ====================

// Test_Vec_PopAfterDestroy tests that Pop returns WatcherStopped error after Destroy is called
func Test_Vec_PopAfterDestroy(t *testing.T) {
	vec := NewVector()
	w := createTestWorker(t)
	vec.Push(w)

	// Destroy the vector
	vec.Destroy()

	// Pop should fail immediately with WatcherStopped
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	popped, err := vec.Pop(ctx)
	assert.Nil(t, popped)
	require.Error(t, err)
	assert.True(t, errors.Is(errors.WatcherStopped, err))
}

// Test_Vec_PopWithCanceledContext tests that Pop returns error when context is canceled
func Test_Vec_PopWithCanceledContext(t *testing.T) {
	vec := NewVector()
	// Don't push any workers - channel is empty

	// Create an already canceled context
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // Cancel immediately

	popped, err := vec.Pop(ctx)
	assert.Nil(t, popped)
	require.Error(t, err)
	assert.True(t, errors.Is(errors.NoFreeWorkers, err))
}

// Test_Vec_PopContextTimeout tests that Pop returns error when context times out waiting for worker
func Test_Vec_PopContextTimeout(t *testing.T) {
	vec := NewVector()
	// Empty channel - no workers to pop

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	popped, err := vec.Pop(ctx)
	elapsed := time.Since(start)

	assert.Nil(t, popped)
	require.Error(t, err)
	assert.True(t, errors.Is(errors.NoFreeWorkers, err))
	// Verify it actually waited for the timeout
	assert.True(t, elapsed >= 50*time.Millisecond, "should have waited for timeout")
}

// ==================== Phase 2: New Tests ====================

// Test_Vec_PopDuringReset_BlocksThenUnblocks verifies Pop blocks during Reset and unblocks after ResetDone.
// If Pop succeeds during reset, a worker can be popped and simultaneously killed by the reset path.
func Test_Vec_PopDuringReset_BlocksThenUnblocks(t *testing.T) {
	vec := NewVector()
	for range 3 {
		vec.Push(createTestWorker(t))
	}

	// Signal reset — Pop should now spin-wait
	vec.Reset()

	type popResult struct {
		w   *worker.Process
		err error
	}
	result := make(chan popResult, 1)

	// Start Pop in a goroutine — it should block in the reset spin-loop
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		w, err := vec.Pop(ctx)
		result <- popResult{w, err}
	}()

	// Verify Pop is blocked (hasn't returned after 300ms)
	select {
	case <-result:
		t.Fatal("Pop should be blocked during reset")
	case <-time.After(300 * time.Millisecond):
		// expected — Pop is blocked in the reset spin-loop
	}

	// Complete reset and push fresh workers
	vec.ResetDone()
	for range 3 {
		vec.Push(createTestWorker(t))
	}

	// Pop should now unblock and succeed
	select {
	case r := <-result:
		require.NoError(t, r.err)
		assert.NotNil(t, r.w)
	case <-time.After(5 * time.Second):
		t.Fatal("Pop should have unblocked after ResetDone")
	}
}

// Test_Vec_PushToFullChannel verifies that when the channel is full, overflow workers are killed.
// Production safety valve — without it, Push goroutine hangs forever on a full channel.
func Test_Vec_PushToFullChannel(t *testing.T) {
	vec := NewVector()

	// Fill channel to capacity (2048)
	for range 2048 {
		vec.Push(createTestWorker(t))
	}
	assert.Equal(t, 2048, vec.Len())

	// Push one more — should hit the default branch and kill the worker (not block).
	// Must be a started worker since Push calls Kill() on overflow.
	done := make(chan struct{})
	go func() {
		vec.Push(createStartedTestWorker(t))
		close(done)
	}()

	select {
	case <-done:
		// Push returned — overflow worker was killed, channel still at capacity
		assert.Equal(t, 2048, vec.Len())
	case <-time.After(2 * time.Second):
		t.Fatal("Push blocked on full channel — safety valve broken")
	}
}

// Test_Vec_Remove_DrainsBadWorkers verifies that Remove drains workers in bad states.
// Remove is called from the worker death callback. If it fails, the pool slowly loses capacity.
func Test_Vec_Remove_DrainsBadWorkers(t *testing.T) {
	vec := NewVector()

	// Push 5 workers: 3 Ready, 2 Errored
	for range 3 {
		w := createTestWorkerInState(t, fsm.StateReady)
		vec.Push(w)
	}
	for range 2 {
		w := createTestWorkerInState(t, fsm.StateErrored)
		vec.Push(w)
	}
	assert.Equal(t, 5, vec.Len())

	// Remove should drain bad workers
	vec.Remove()

	// Only Ready workers should remain
	assert.Equal(t, 3, vec.Len())
}

// Test_Vec_ConcurrentPushPop verifies no panics or deadlocks under concurrent Push/Pop.
// In production, Exec releases workers (Push) while new Exec calls take workers (Pop) continuously.
func Test_Vec_ConcurrentPushPop(t *testing.T) {
	vec := NewVector()
	const producers = 5
	const consumers = 5
	const opsPerGoroutine = 50

	// Seed with some workers so consumers don't starve
	for range 20 {
		vec.Push(createTestWorker(t))
	}

	var wg sync.WaitGroup

	// Producer goroutines
	for range producers {
		wg.Go(func() {
			for range opsPerGoroutine {
				vec.Push(createTestWorker(t))
			}
		})
	}

	// Consumer goroutines
	for range consumers {
		wg.Go(func() {
			for range opsPerGoroutine {
				popCtx, popCancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				_, _ = vec.Pop(popCtx)
				popCancel()
			}
		})
	}

	wg.Wait()
	assert.GreaterOrEqual(t, vec.Len(), 0)
}
