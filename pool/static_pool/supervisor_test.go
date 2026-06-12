package static_pool

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/roadrunner-server/pool/v2/fsm"
	"github.com/roadrunner-server/pool/v2/pool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHelperProcess is not a real test: it is re-executed by the supervisor tests
// as a worker process that steadily grows its memory usage.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_POOL_HELPER") != "memhog" {
		return
	}

	hog := make([][]byte, 0, 100)
	for range 100 {
		b := make([]byte, 1<<20)
		for i := range b {
			b[i] = byte(i)
		}
		hog = append(hog, b)
		time.Sleep(time.Millisecond * 5)
	}
	runtime.KeepAlive(hog)
	select {}
}

func memhogCmd(_ []string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess$") //nolint:gosec // re-executes the test binary itself
	cmd.Env = append(os.Environ(), "GO_POOL_HELPER=memhog")
	return cmd
}

func supervisedCfg(s *pool.SupervisorConfig) *pool.Config {
	return &pool.Config{
		NumWorkers:      1,
		AllocateTimeout: time.Second * 10,
		DestroyTimeout:  time.Second * 10,
		Supervisor:      s,
	}
}

func Test_SupervisedPool_TTL_WorkerRestarted(t *testing.T) {
	cfg := supervisedCfg(&pool.SupervisorConfig{
		WatchTick: time.Millisecond * 200,
		TTL:       time.Second,
	})

	p, err := NewPool(t.Context(), sleepCmd, cfg, slog.Default())
	require.NoError(t, err)

	oldPid := p.Workers()[0].Pid()

	// after the TTL elapses the worker is stopped and replaced
	require.Eventually(t, func() bool {
		workers := p.Workers()
		return len(workers) == 1 && workers[0].Pid() != oldPid && workers[0].State().Compare(fsm.StateReady)
	}, 10*time.Second, 100*time.Millisecond)

	p.Destroy(context.Background())
}

func Test_SupervisedPool_MaxMemoryReached(t *testing.T) {
	cfg := supervisedCfg(&pool.SupervisorConfig{
		WatchTick:       time.Millisecond * 200,
		MaxWorkerMemory: 32, // MB; the helper grows to ~100MB
	})

	p, err := NewPool(t.Context(), memhogCmd, cfg, slog.Default())
	require.NoError(t, err)

	oldPid := p.Workers()[0].Pid()

	require.Eventually(t, func() bool {
		workers := p.Workers()
		return len(workers) == 1 && workers[0].Pid() != oldPid
	}, 15*time.Second, 100*time.Millisecond)

	p.Destroy(context.Background())
}

func Test_SupervisedPool_IdleTTL_SkipsNeverUsedWorker(t *testing.T) {
	cfg := supervisedCfg(&pool.SupervisorConfig{
		WatchTick: time.Millisecond * 200,
		IdleTTL:   time.Second,
	})

	p, err := NewPool(t.Context(), sleepCmd, cfg, slog.Default())
	require.NoError(t, err)

	oldPid := p.Workers()[0].Pid()

	// LastUsed == 0 (never marked as used): the idle check must not fire
	time.Sleep(time.Second * 2)

	workers := p.Workers()
	require.Len(t, workers, 1)
	assert.Equal(t, oldPid, workers[0].Pid())
	assert.True(t, workers[0].State().Compare(fsm.StateReady))

	p.Destroy(context.Background())
}

func Test_SupervisedPool_IdleTTL_Fires(t *testing.T) {
	cfg := supervisedCfg(&pool.SupervisorConfig{
		WatchTick: time.Millisecond * 200,
		IdleTTL:   time.Second,
	})

	p, err := NewPool(t.Context(), sleepCmd, cfg, slog.Default())
	require.NoError(t, err)

	w := p.Workers()[0]
	oldPid := w.Pid()
	// mark the worker as last used an hour ago; activity tracking is plugin-side now
	w.State().SetLastUsed(uint64(time.Now().Add(-time.Hour).UnixNano()))

	require.Eventually(t, func() bool {
		workers := p.Workers()
		return len(workers) == 1 && workers[0].Pid() != oldPid && workers[0].State().Compare(fsm.StateReady)
	}, 10*time.Second, 100*time.Millisecond)

	p.Destroy(context.Background())
}

func Test_SupervisedPool_AddRemoveWorkers(t *testing.T) {
	cfg := supervisedCfg(&pool.SupervisorConfig{
		WatchTick: time.Second,
		TTL:       time.Second * 100,
	})

	p, err := NewPool(t.Context(), sleepCmd, cfg, slog.Default())
	require.NoError(t, err)

	require.NoError(t, p.AddWorker())
	require.Len(t, p.Workers(), 2)

	require.NoError(t, p.RemoveWorker(context.Background()))
	require.Len(t, p.Workers(), 1)

	p.Destroy(context.Background())
}

func Test_SupervisedPool_ImmediateDestroy(t *testing.T) {
	cfg := supervisedCfg(&pool.SupervisorConfig{
		WatchTick: time.Second,
		TTL:       time.Second * 100,
	})

	p, err := NewPool(t.Context(), sleepCmd, cfg, slog.Default())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Destroy(ctx)
}

func Test_SupervisedPool_Reset(t *testing.T) {
	cfg := supervisedCfg(&pool.SupervisorConfig{
		WatchTick: time.Second,
		TTL:       time.Second * 100,
	})
	cfg.ResetTimeout = time.Second * 10

	p, err := NewPool(t.Context(), sleepCmd, cfg, slog.Default())
	require.NoError(t, err)

	oldPid := p.Workers()[0].Pid()

	require.NoError(t, p.Reset(context.Background()))

	workers := p.Workers()
	require.Len(t, workers, 1)
	assert.NotEqual(t, oldPid, workers[0].Pid())

	p.Destroy(context.Background())
}
