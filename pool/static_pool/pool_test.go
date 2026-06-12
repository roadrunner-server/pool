package static_pool

import (
	"context"
	"log/slog"
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/roadrunner-server/pool/v2/fsm"
	"github.com/roadrunner-server/pool/v2/pool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sleepCmd is a long-lived worker stand-in: no IPC, just an OS process
func sleepCmd(_ []string) *exec.Cmd {
	return exec.Command("sleep", "300")
}

func testCfg() *pool.Config {
	return &pool.Config{
		NumWorkers:      2,
		AllocateTimeout: time.Second * 10,
		DestroyTimeout:  time.Second * 10,
	}
}

func Test_NewPool(t *testing.T) {
	p, err := NewPool(t.Context(), sleepCmd, testCfg(), slog.Default())
	require.NoError(t, err)
	require.NotNil(t, p)

	workers := p.Workers()
	require.Len(t, workers, 2)
	for i := range workers {
		assert.True(t, workers[i].State().Compare(fsm.StateReady))
		assert.NotZero(t, workers[i].Pid())
	}

	p.Destroy(context.Background())
}

func Test_NewPool_NilConfig(t *testing.T) {
	_, err := NewPool(t.Context(), sleepCmd, nil, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil configuration provided")
}

func Test_NewPool_MaxWorkers(t *testing.T) {
	cfg := testCfg()
	cfg.NumWorkers = 501
	_, err := NewPool(t.Context(), sleepCmd, cfg, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "number of workers can't be more than 500")
}

func Test_NewPool_WrongCommand(t *testing.T) {
	cmd := func(_ []string) *exec.Cmd {
		return exec.Command("/non/existent/binary")
	}

	// without the IPC handshake, a bad binary fails at cmd.Start()
	_, err := NewPool(t.Context(), cmd, testCfg(), slog.Default())
	require.Error(t, err)
}

func Test_NewPool_GetConfig(t *testing.T) {
	cfg := testCfg()
	p, err := NewPool(t.Context(), sleepCmd, cfg, slog.Default())
	require.NoError(t, err)

	assert.Same(t, cfg, p.GetConfig())
	assert.Equal(t, uint64(0), p.NumDynamic())

	p.Destroy(context.Background())
}

func Test_NewPoolAddRemoveWorkers(t *testing.T) {
	p, err := NewPool(t.Context(), sleepCmd, testCfg(), slog.Default())
	require.NoError(t, err)

	require.NoError(t, p.AddWorker())
	require.Len(t, p.Workers(), 3)

	require.NoError(t, p.RemoveWorker(context.Background()))
	require.Len(t, p.Workers(), 2)

	p.Destroy(context.Background())
}

func Test_NewPool_RemoveLastWorker(t *testing.T) {
	cfg := testCfg()
	cfg.NumWorkers = 1
	p, err := NewPool(t.Context(), sleepCmd, cfg, slog.Default())
	require.NoError(t, err)

	// the last worker can't be removed
	require.NoError(t, p.RemoveWorker(context.Background()))
	require.Len(t, p.Workers(), 1)

	p.Destroy(context.Background())
}

func Test_NewPoolReset(t *testing.T) {
	cfg := testCfg()
	cfg.ResetTimeout = time.Second * 10
	p, err := NewPool(t.Context(), sleepCmd, cfg, slog.Default())
	require.NoError(t, err)

	oldPids := map[int64]struct{}{}
	for _, w := range p.Workers() {
		oldPids[w.Pid()] = struct{}{}
	}
	require.Len(t, oldPids, 2)

	require.NoError(t, p.Reset(context.Background()))

	workers := p.Workers()
	require.Len(t, workers, 2)
	for _, w := range workers {
		assert.NotContains(t, oldPids, w.Pid())
	}

	p.Destroy(context.Background())
}

func Test_StaticPool_ImmediateDestroy(t *testing.T) {
	p, err := NewPool(t.Context(), sleepCmd, testCfg(), slog.Default())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// already canceled context: workers are stopped via the ctx.Done path
	p.Destroy(ctx)
}

func Test_Static_Pool_Handle_Dead(t *testing.T) {
	cfg := testCfg()
	cfg.NumWorkers = 1
	p, err := NewPool(t.Context(), sleepCmd, cfg, slog.Default())
	require.NoError(t, err)

	oldPid := p.Workers()[0].Pid()
	require.NotZero(t, oldPid)

	// kill the worker process behind the pool's back
	require.NoError(t, syscall.Kill(int(oldPid), syscall.SIGKILL))

	// the watcher must observe the death and allocate a replacement
	require.Eventually(t, func() bool {
		workers := p.Workers()
		return len(workers) == 1 && workers[0].Pid() != oldPid && workers[0].State().Compare(fsm.StateReady)
	}, 10*time.Second, 50*time.Millisecond)

	p.Destroy(context.Background())
}

func Test_NewPool_InstantExitWorker(t *testing.T) {
	// without the IPC handshake the spawn of an instantly-exiting worker succeeds;
	// the death is observed by the watcher, which keeps re-allocating.
	// After a few exits the command turns long-lived so the churn settles and
	// the pool can be destroyed cleanly.
	var spawns atomic.Uint64
	cmd := func(_ []string) *exec.Cmd {
		if spawns.Add(1) <= 3 {
			return exec.Command("sh", "-c", "exit 1")
		}
		return exec.Command("sleep", "300")
	}

	cfg := testCfg()
	cfg.NumWorkers = 1
	p, err := NewPool(t.Context(), cmd, cfg, slog.Default())
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		workers := p.Workers()
		return spawns.Load() >= 4 && len(workers) == 1 && workers[0].State().Compare(fsm.StateReady)
	}, 10*time.Second, 50*time.Millisecond)

	p.Destroy(context.Background())
}
