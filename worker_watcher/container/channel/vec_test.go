package channel

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/pool/worker"
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

// ==================== General Tests ====================

// Test_Vec_PushPop tests basic push and pop operations
func Test_Vec_PushPop(t *testing.T) {
	vec := NewVector()
	w := createTestWorker(t)

	// Push worker
	vec.Push(w)
	assert.Equal(t, 1, vec.Len())

	// Pop worker
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
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
	ctx, cancel := context.WithCancel(context.Background())
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

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
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
