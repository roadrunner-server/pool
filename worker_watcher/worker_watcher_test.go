package worker_watcher

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/pool/v2/fsm"
	"github.com/roadrunner-server/pool/v2/worker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// alwaysFailAllocator returns an allocator that always fails.
func alwaysFailAllocator() Allocator {
	return func() (*worker.Process, error) {
		return nil, fmt.Errorf("permanent allocation failure")
	}
}

// fakeAllocator returns an allocator that fails failCount times then succeeds.
// Counter semantics: Store(2) means "fail twice" because Add(-1) returns the new value,
// so call 1 returns 1 (>=0, fail), call 2 returns 0 (>=0, fail), call 3 returns -1 (<0, succeed).
// Workers are created with a running "sleep" process so Kill won't panic.
// Note: workers won't have a relay, so Stop() will panic — tests must avoid Stop paths.
func fakeAllocator(t *testing.T, failCount *atomic.Int32) Allocator {
	t.Helper()
	return func() (*worker.Process, error) {
		if failCount.Add(-1) >= 0 {
			return nil, fmt.Errorf("simulated allocation failure")
		}
		cmd := exec.Command("sleep", "100")
		w, err := worker.InitBaseWorker(cmd)
		if err != nil {
			return nil, err
		}
		if err = cmd.Start(); err != nil {
			return nil, err
		}
		t.Cleanup(func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
		w.State().Transition(fsm.StateReady)
		return w, nil
	}
}

// createStartedWorker creates a worker with a running process in the specified state.
// Note: no relay attached — Kill() works but Stop() will panic.
func createStartedWorker(t *testing.T, state int64) *worker.Process {
	t.Helper()
	cmd := exec.Command("sleep", "100")
	w, err := worker.InitBaseWorker(cmd)
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

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
	}
	return w
}

// shutdownWatcher signals the watcher to stop (without calling Destroy which needs relay).
func shutdownWatcher(ww *WorkerWatcher) {
	ww.container.Destroy()
	select {
	case ww.stopCh <- struct{}{}:
	default:
	}
}

// TestWorkerWatcher_AllocateRetryTimeout verifies that Allocate returns a WorkerAllocate error
// after the allocateTimeout when allocation always fails.
// If the retry loop hangs, the ww state is corrupted and the pool deadlocks.
func TestWorkerWatcher_AllocateRetryTimeout(t *testing.T) {
	log := slog.Default()
	ww := NewSyncWorkerWatcher(alwaysFailAllocator(), log, 1, 3*time.Second)
	t.Cleanup(func() { shutdownWatcher(ww) })

	start := time.Now()
	err := ww.Allocate()
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, errors.Is(errors.WorkerAllocate, err))
	assert.GreaterOrEqual(t, elapsed, 3*time.Second, "should have retried for ~3s")
	assert.Less(t, elapsed, 8*time.Second, "should not exceed timeout significantly")
}

// TestWorkerWatcher_AllocateNoTimeout verifies that Allocate returns immediately
// when allocateTimeout is 0 (no retry).
func TestWorkerWatcher_AllocateNoTimeout(t *testing.T) {
	log := slog.Default()
	ww := NewSyncWorkerWatcher(alwaysFailAllocator(), log, 1, 0)
	t.Cleanup(func() { shutdownWatcher(ww) })

	start := time.Now()
	err := ww.Allocate()
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, errors.Is(errors.WorkerAllocate, err))
	assert.Less(t, elapsed, time.Second, "should return immediately with no retry timeout")
}

// TestWorkerWatcher_AllocateRetrySuccess verifies that Allocate retries and eventually succeeds.
// Validates the goto done path — ticker is stopped, worker is added to watch and container.
func TestWorkerWatcher_AllocateRetrySuccess(t *testing.T) {
	log := slog.Default()
	var failCount atomic.Int32
	failCount.Store(2) // fail twice, succeed on third

	ww := NewSyncWorkerWatcher(fakeAllocator(t, &failCount), log, 1, 10*time.Second)
	t.Cleanup(func() { shutdownWatcher(ww) })

	err := ww.Allocate()
	require.NoError(t, err)

	// Worker should now be in the watcher's list
	workers := ww.List()
	assert.Len(t, workers, 1)
	assert.Equal(t, fsm.StateReady, workers[0].State().CurrentState())
}

// TestWorkerWatcher_RemoveWorker_CannotRemoveLast verifies the last worker cannot be removed.
// The numWorkers==1 guard in RemoveWorker() prevents removing the last worker. If broken, pool panics on "no workers available".
func TestWorkerWatcher_RemoveWorker_CannotRemoveLast(t *testing.T) {
	log := slog.Default()
	ww := NewSyncWorkerWatcher(alwaysFailAllocator(), log, 1, 5*time.Second)
	t.Cleanup(func() { shutdownWatcher(ww) })

	// numWorkers is 1 (set in constructor). RemoveWorker should refuse.
	assert.Equal(t, uint64(1), ww.numWorkers.Load())

	// RemoveWorker internally calls Take which needs workers in the container,
	// but the numWorkers==1 guard in RemoveWorker() checks first and returns nil.
	err := ww.RemoveWorker(t.Context())
	require.NoError(t, err)
	assert.Equal(t, uint64(1), ww.numWorkers.Load(), "should not decrement the last worker")
}

// TestWorkerWatcher_Take_FastPath_ReadyWorker verifies Take returns immediately for Ready workers.
func TestWorkerWatcher_Take_FastPath_ReadyWorker(t *testing.T) {
	log := slog.Default()
	ww := NewSyncWorkerWatcher(alwaysFailAllocator(), log, 1, 0)
	t.Cleanup(func() { shutdownWatcher(ww) })

	w := createStartedWorker(t, fsm.StateReady)
	ww.container.Push(w)

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	got, err := ww.Take(ctx)
	require.NoError(t, err)
	assert.Equal(t, w.Pid(), got.Pid())
	assert.Equal(t, fsm.StateReady, got.State().CurrentState())
}

// TestWorkerWatcher_Take_SlowPath_SkipsBadWorkers verifies Take skips non-Ready workers
// and kills them. The first non-Ready worker triggers the slow path via Kill() (works without relay).
// Subsequent bad workers are killed via Stop() — we avoid that by only having one bad + one good.
func TestWorkerWatcher_Take_SlowPath_SkipsBadWorkers(t *testing.T) {
	log := slog.Default()
	ww := NewSyncWorkerWatcher(alwaysFailAllocator(), log, 2, 0)
	t.Cleanup(func() { shutdownWatcher(ww) })

	// First worker: TTL'd (not Ready) — triggers slow path, gets killed via Kill()
	w1 := createStartedWorker(t, fsm.StateTTLReached)
	// Second worker: Ready — should be returned
	w2 := createStartedWorker(t, fsm.StateReady)

	ww.container.Push(w1)
	ww.container.Push(w2)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	got, err := ww.Take(ctx)
	require.NoError(t, err)
	assert.Equal(t, w2.Pid(), got.Pid())
	assert.Equal(t, fsm.StateReady, got.State().CurrentState())
}

// TestWorkerWatcher_Release_ReadyState_PushesToContainer verifies releasing a Ready worker
// pushes it back to the container for reuse.
func TestWorkerWatcher_Release_ReadyState_PushesToContainer(t *testing.T) {
	log := slog.Default()
	ww := NewSyncWorkerWatcher(alwaysFailAllocator(), log, 1, 0)
	t.Cleanup(func() { shutdownWatcher(ww) })

	w := createStartedWorker(t, fsm.StateReady)
	initialLen := ww.container.Len()

	ww.Release(w)

	assert.Equal(t, initialLen+1, ww.container.Len(), "ready worker should be pushed to container")
}

// TestWorkerWatcher_Take_AfterDestroy verifies Take returns WatcherStopped after container is destroyed.
func TestWorkerWatcher_Take_AfterDestroy(t *testing.T) {
	log := slog.Default()
	ww := NewSyncWorkerWatcher(alwaysFailAllocator(), log, 1, 0)

	// Push a worker and destroy the container
	w := createStartedWorker(t, fsm.StateReady)
	ww.container.Push(w)
	ww.container.Destroy()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	_, err := ww.Take(ctx)
	require.Error(t, err)
	assert.True(t, errors.Is(errors.WatcherStopped, err))
}

// TestWorkerWatcher_List_ReturnsAllWorkers verifies List returns a copy of all tracked workers.
func TestWorkerWatcher_List_ReturnsAllWorkers(t *testing.T) {
	log := slog.Default()
	ww := NewSyncWorkerWatcher(alwaysFailAllocator(), log, 3, 0)
	t.Cleanup(func() { shutdownWatcher(ww) })

	// Store 3 workers directly in the workers map with unique keys.
	// We use synthetic keys because our fake workers all have pid=0
	// (they bypass w.Start() which sets the pid field).
	for i := range 3 {
		w := createStartedWorker(t, fsm.StateReady)
		ww.workers.Store(int64(i+1), w)
	}

	workers := ww.List()
	assert.Len(t, workers, 3)
}

// TestWorkerWatcher_List_EmptyWatcher verifies List returns nil when numWorkers is 0.
func TestWorkerWatcher_List_EmptyWatcher(t *testing.T) {
	log := slog.Default()
	ww := NewSyncWorkerWatcher(alwaysFailAllocator(), log, 0, 0)
	t.Cleanup(func() { shutdownWatcher(ww) })

	workers := ww.List()
	assert.Nil(t, workers)
}

// TestWorkerWatcher_Allocate_StopChExitsRetryLoop verifies that sending on stopCh
// causes the Allocate retry loop to exit promptly with WatcherStopped.
func TestWorkerWatcher_Allocate_StopChExitsRetryLoop(t *testing.T) {
	log := slog.Default()
	ww := NewSyncWorkerWatcher(alwaysFailAllocator(), log, 1, 10*time.Second)

	// Send on stopCh after 1 second (in the background)
	go func() {
		time.Sleep(time.Second)
		ww.stopCh <- struct{}{}
	}()

	start := time.Now()
	err := ww.Allocate()
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, errors.Is(errors.WatcherStopped, err))
	assert.Less(t, elapsed, 3*time.Second, "should exit promptly via stopCh")
}
