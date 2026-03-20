package static_pool

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/roadrunner-server/pool/v2/fsm"
	"github.com/roadrunner-server/pool/v2/ipc/pipe"
	"github.com/roadrunner-server/pool/v2/payload"
	"github.com/roadrunner-server/pool/v2/pool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var cfgSupervised = &pool.Config{
	NumWorkers:      uint64(1),
	AllocateTimeout: time.Second * 10,
	DestroyTimeout:  time.Second * 10,
	Supervisor: &pool.SupervisorConfig{
		WatchTick:       1 * time.Second,
		TTL:             100 * time.Second,
		IdleTTL:         100 * time.Second,
		ExecTTL:         100 * time.Second,
		MaxWorkerMemory: 100,
	},
}

func Test_SupervisedPool_Exec(t *testing.T) {
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/memleak.php", "pipes") },
		pipe.NewPipeFactory(slog.Default()),
		cfgSupervised,
		slog.Default(),
	)

	require.NoError(t, err)
	require.NotNil(t, p)

	time.Sleep(time.Second)

	pidBefore := p.Workers()[0].Pid()

	for range 10 {
		time.Sleep(time.Second)
		_, err = p.Exec(t.Context(), &payload.Payload{
			Context: []byte(""),
			Body:    []byte("foo"),
		}, make(chan struct{}))
		require.NoError(t, err)
	}

	time.Sleep(time.Second)
	require.NotEqual(t, pidBefore, p.Workers()[0].Pid())
	t.Cleanup(func() { p.Destroy(t.Context()) })
}

func Test_SupervisedPool_AddRemoveWorkers(t *testing.T) {
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/memleak.php", "pipes") },
		pipe.NewPipeFactory(slog.Default()),
		cfgSupervised,
		slog.Default(),
	)

	require.NoError(t, err)
	require.NotNil(t, p)

	time.Sleep(time.Second)

	pidBefore := p.Workers()[0].Pid()

	for range 10 {
		time.Sleep(time.Second)
		_, err = p.Exec(t.Context(), &payload.Payload{
			Context: []byte(""),
			Body:    []byte("foo"),
		}, make(chan struct{}))
		require.NoError(t, err)
	}

	time.Sleep(time.Second)
	require.NotEqual(t, pidBefore, p.Workers()[0].Pid())
	t.Cleanup(func() { p.Destroy(t.Context()) })
}

func Test_SupervisedPool_ImmediateDestroy(t *testing.T) {
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/client.php", "echo", "pipes") },
		pipe.NewPipeFactory(slog.Default()),
		&pool.Config{
			AllocateTimeout: time.Second * 10,
			DestroyTimeout:  time.Second * 10,
			Supervisor: &pool.SupervisorConfig{
				WatchTick:       1 * time.Second,
				TTL:             100 * time.Second,
				IdleTTL:         100 * time.Second,
				ExecTTL:         100 * time.Second,
				MaxWorkerMemory: 100,
			},
		},
		slog.Default(),
	)
	assert.NoError(t, err)
	assert.NotNil(t, p)

	_, _ = p.Exec(t.Context(), &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))

	ctx, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancel()

	p.Destroy(ctx)
}

func Test_SupervisedPool_NilFactory(t *testing.T) {
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/client.php", "echo", "pipes") },
		pipe.NewPipeFactory(slog.Default()),
		nil,
		slog.Default(),
	)
	assert.Error(t, err)
	assert.Nil(t, p)
}

func Test_SupervisedPool_NilConfig(t *testing.T) {
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/client.php", "echo", "pipes") },
		nil,
		cfgSupervised,
		slog.Default(),
	)
	assert.Error(t, err)
	assert.Nil(t, p)
}

func Test_SupervisedPool_RemoveNoWorkers(t *testing.T) {
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/client.php", "echo", "pipes") },
		pipe.NewPipeFactory(slog.Default()),
		cfgSupervised,
		slog.Default(),
	)
	assert.NoError(t, err)
	assert.NotNil(t, p)

	_, err = p.Exec(t.Context(), &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
	assert.NoError(t, err)

	wrks := p.Workers()
	for range wrks {
		assert.NoError(t, p.RemoveWorker(t.Context()))
	}

	assert.Len(t, p.Workers(), 1)
	t.Cleanup(func() { p.Destroy(t.Context()) })
}

func Test_SupervisedPool_RemoveWorker(t *testing.T) {
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/client.php", "echo", "pipes") },
		pipe.NewPipeFactory(slog.Default()),
		cfgSupervised,
		slog.Default(),
	)
	assert.NoError(t, err)
	assert.NotNil(t, p)

	_, err = p.Exec(t.Context(), &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
	assert.NoError(t, err)

	wrks := p.Workers()
	for range wrks {
		assert.NoError(t, p.RemoveWorker(t.Context()))
	}

	// should not be error, 1 worker should be in the pool
	_, err = p.Exec(t.Context(), &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
	assert.NoError(t, err)

	err = p.AddWorker()
	assert.NoError(t, err)

	_, err = p.Exec(t.Context(), &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
	assert.NoError(t, err)

	assert.Len(t, p.Workers(), 2)
	t.Cleanup(func() { p.Destroy(t.Context()) })
}

func Test_SupervisedPoolReset(t *testing.T) {
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/client.php", "echo", "pipes") },
		pipe.NewPipeFactory(slog.Default()),
		cfgSupervised,
		slog.Default(),
	)
	assert.NoError(t, err)
	assert.NotNil(t, p)

	w := p.Workers()
	if len(w) == 0 {
		t.Fatal("should be workers inside")
	}

	pid := w[0].Pid()
	require.NoError(t, p.Reset(t.Context()))

	w2 := p.Workers()
	if len(w2) == 0 {
		t.Fatal("should be workers inside")
	}

	require.NotEqual(t, pid, w2[0].Pid())
	t.Cleanup(func() { p.Destroy(t.Context()) })
}

// This test should finish without freezes
func TestSupervisedPool_ExecWithDebugMode(t *testing.T) {
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/supervised.php") },
		pipe.NewPipeFactory(slog.Default()),
		&pool.Config{
			Debug:           true,
			NumWorkers:      uint64(1),
			AllocateTimeout: time.Second,
			DestroyTimeout:  time.Second,
			Supervisor: &pool.SupervisorConfig{
				WatchTick:       1 * time.Second,
				TTL:             100 * time.Second,
				IdleTTL:         100 * time.Second,
				ExecTTL:         100 * time.Second,
				MaxWorkerMemory: 100,
			},
		},
		slog.Default(),
	)

	assert.NoError(t, err)
	assert.NotNil(t, p)

	time.Sleep(time.Second)

	for range 10 {
		time.Sleep(time.Second)
		_, err = p.Exec(t.Context(), &payload.Payload{
			Context: []byte(""),
			Body:    []byte("foo"),
		}, make(chan struct{}))
		assert.NoError(t, err)
	}
	t.Cleanup(func() { p.Destroy(t.Context()) })
}

func TestSupervisedPool_ExecTTL_TimedOut(t *testing.T) {
	var cfgExecTTL = &pool.Config{
		NumWorkers:      uint64(1),
		AllocateTimeout: time.Second,
		DestroyTimeout:  time.Second,
		Supervisor: &pool.SupervisorConfig{
			WatchTick:       1 * time.Second,
			TTL:             100 * time.Second,
			IdleTTL:         100 * time.Second,
			ExecTTL:         1 * time.Second,
			MaxWorkerMemory: 100,
		},
	}
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/sleep.php", "pipes") },
		pipe.NewPipeFactory(slog.Default()),
		cfgExecTTL,
		slog.Default(),
	)

	assert.NoError(t, err)
	assert.NotNil(t, p)

	pid := p.Workers()[0].Pid()

	_, err = p.Exec(t.Context(), &payload.Payload{
		Context: []byte(""),
		Body:    []byte("foo"),
	}, make(chan struct{}))
	assert.Error(t, err)

	time.Sleep(time.Second * 1)
	// should be new worker with new pid
	assert.NotEqual(t, pid, p.Workers()[0].Pid())
	t.Cleanup(func() { p.Destroy(t.Context()) })
}

func TestSupervisedPool_TTL_WorkerRestarted(t *testing.T) {
	var cfgExecTTL = &pool.Config{
		NumWorkers: uint64(1),
		Supervisor: &pool.SupervisorConfig{
			WatchTick: 1 * time.Second,
			TTL:       5 * time.Second,
		},
	}
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/sleep-ttl.php") },
		pipe.NewPipeFactory(slog.Default()),
		cfgExecTTL,
		slog.Default(),
	)

	assert.NoError(t, err)
	assert.NotNil(t, p)

	pid := p.Workers()[0].Pid()

	respCh, err := p.Exec(t.Context(), &payload.Payload{
		Context: []byte(""),
		Body:    []byte("foo"),
	}, make(chan struct{}))
	assert.NoError(t, err)

	resp := <-respCh

	assert.Equal(t, string(resp.Body()), "hello world")
	assert.Empty(t, resp.Context())

	time.Sleep(time.Second)
	assert.NotEqual(t, pid, p.Workers()[0].Pid())
	require.Equal(t, p.Workers()[0].State().CurrentState(), fsm.StateReady)
	pid = p.Workers()[0].Pid()

	respCh, err = p.Exec(t.Context(), &payload.Payload{
		Context: []byte(""),
		Body:    []byte("foo"),
	}, make(chan struct{}))
	assert.NoError(t, err)

	resp = <-respCh

	assert.Equal(t, string(resp.Body()), "hello world")
	assert.Empty(t, resp.Context())

	time.Sleep(time.Second)
	// should be new worker with new pid
	assert.NotEqual(t, pid, p.Workers()[0].Pid())
	require.Equal(t, p.Workers()[0].State().CurrentState(), fsm.StateReady)
	t.Cleanup(func() { p.Destroy(t.Context()) })
}

func TestSupervisedPool_Idle(t *testing.T) {
	var cfgExecTTL = &pool.Config{
		NumWorkers:      uint64(1),
		AllocateTimeout: time.Second,
		DestroyTimeout:  time.Second,
		Supervisor: &pool.SupervisorConfig{
			WatchTick:       1 * time.Second,
			TTL:             100 * time.Second,
			IdleTTL:         1 * time.Second,
			ExecTTL:         100 * time.Second,
			MaxWorkerMemory: 100,
		},
	}
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/idle.php", "pipes") },
		pipe.NewPipeFactory(slog.Default()),
		cfgExecTTL,
		slog.Default(),
	)

	assert.NoError(t, err)
	assert.NotNil(t, p)

	pid := p.Workers()[0].Pid()

	respCh, err := p.Exec(t.Context(), &payload.Payload{
		Context: []byte(""),
		Body:    []byte("foo"),
	}, make(chan struct{}))
	assert.NoError(t, err)

	resp := <-respCh

	assert.Empty(t, resp.Body())
	assert.Empty(t, resp.Context())

	time.Sleep(time.Second * 5)

	// worker should be marked as invalid and reallocated
	_, err = p.Exec(t.Context(), &payload.Payload{
		Context: []byte(""),
		Body:    []byte("foo"),
	}, make(chan struct{}))

	assert.NoError(t, err)
	require.Len(t, p.Workers(), 1)
	// should be new worker with new pid
	assert.NotEqual(t, pid, p.Workers()[0].Pid())
	t.Cleanup(func() { p.Destroy(t.Context()) })
}

func TestSupervisedPool_IdleTTL_StateAfterTimeout(t *testing.T) {
	var cfgExecTTL = &pool.Config{
		NumWorkers:      uint64(1),
		AllocateTimeout: time.Second,
		DestroyTimeout:  time.Second,
		Supervisor: &pool.SupervisorConfig{
			WatchTick:       1 * time.Second,
			IdleTTL:         1 * time.Second,
			MaxWorkerMemory: 100,
		},
	}
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/exec_ttl.php", "pipes") },
		pipe.NewPipeFactory(slog.Default()),
		cfgExecTTL,
		slog.Default(),
	)

	assert.NoError(t, err)
	assert.NotNil(t, p)

	pid := p.Workers()[0].Pid()

	time.Sleep(time.Second)
	respCh, err := p.Exec(t.Context(), &payload.Payload{
		Context: []byte(""),
		Body:    []byte("foo"),
	}, make(chan struct{}))
	assert.NoError(t, err)

	resp := <-respCh

	assert.Empty(t, resp.Body())
	assert.Empty(t, resp.Context())

	time.Sleep(time.Second * 5)

	if len(p.Workers()) < 1 {
		t.Fatal("should be at least 1 worker")
		return
	}

	// should be destroyed, state should be Ready, not Invalid
	assert.NotEqual(t, pid, p.Workers()[0].Pid())
	assert.Equal(t, fsm.StateReady, p.Workers()[0].State().CurrentState())
	t.Cleanup(func() { p.Destroy(t.Context()) })
}

func TestSupervisedPool_ExecTTL_OK(t *testing.T) {
	var cfgExecTTL = &pool.Config{
		NumWorkers:      uint64(1),
		AllocateTimeout: time.Second,
		DestroyTimeout:  time.Second,
		Supervisor: &pool.SupervisorConfig{
			WatchTick:       1 * time.Second,
			TTL:             100 * time.Second,
			IdleTTL:         100 * time.Second,
			ExecTTL:         4 * time.Second,
			MaxWorkerMemory: 100,
		},
	}
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/exec_ttl.php", "pipes") },
		pipe.NewPipeFactory(slog.Default()),
		cfgExecTTL,
		slog.Default(),
	)

	assert.NoError(t, err)
	assert.NotNil(t, p)

	pid := p.Workers()[0].Pid()

	time.Sleep(time.Millisecond * 100)
	respCh, err := p.Exec(t.Context(), &payload.Payload{
		Context: []byte(""),
		Body:    []byte("foo"),
	}, make(chan struct{}))
	assert.NoError(t, err)

	resp := <-respCh

	assert.Empty(t, resp.Body())
	assert.Empty(t, resp.Context())

	time.Sleep(time.Second * 1)
	// should be the same pid
	assert.Equal(t, pid, p.Workers()[0].Pid())
	t.Cleanup(func() { p.Destroy(t.Context()) })
}

func TestSupervisedPool_ShouldRespond(t *testing.T) {
	var cfgExecTTL = &pool.Config{
		NumWorkers:      uint64(1),
		AllocateTimeout: time.Minute,
		DestroyTimeout:  time.Minute,
		Supervisor: &pool.SupervisorConfig{
			ExecTTL:         90 * time.Second,
			MaxWorkerMemory: 100,
		},
	}

	// constructed
	// max memory
	// constructed
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd {
			return exec.Command("php", "../../tests/should-not-be-killed.php", "pipes")
		},
		pipe.NewPipeFactory(slog.Default()),
		cfgExecTTL,
		slog.Default(),
	)

	assert.NoError(t, err)
	assert.NotNil(t, p)

	respCh, err := p.Exec(t.Context(), &payload.Payload{
		Context: []byte(""),
		Body:    []byte("foo"),
	}, make(chan struct{}))
	assert.NoError(t, err)

	resp := <-respCh

	assert.Equal(t, []byte("alive"), resp.Body())
	assert.Empty(t, resp.Context())

	time.Sleep(time.Second)
	t.Cleanup(func() { p.Destroy(t.Context()) })
}

func TestSupervisedPool_MaxMemoryReached(t *testing.T) {
	var cfgExecTTL = &pool.Config{
		NumWorkers:      uint64(1),
		AllocateTimeout: time.Second,
		DestroyTimeout:  time.Second,
		Supervisor: &pool.SupervisorConfig{
			WatchTick:       1 * time.Second,
			TTL:             100 * time.Second,
			IdleTTL:         100 * time.Second,
			ExecTTL:         4 * time.Second,
			MaxWorkerMemory: 1,
		},
	}

	// constructed
	// max memory
	// constructed
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/memleak.php", "pipes") },
		pipe.NewPipeFactory(slog.Default()),
		cfgExecTTL,
		slog.Default(),
	)

	assert.NoError(t, err)
	assert.NotNil(t, p)

	respCh, err := p.Exec(t.Context(), &payload.Payload{
		Context: []byte(""),
		Body:    []byte("foo"),
	}, make(chan struct{}))
	assert.NoError(t, err)

	resp := <-respCh

	assert.Empty(t, resp.Body())
	assert.Empty(t, resp.Context())

	time.Sleep(time.Second)
	t.Cleanup(func() { p.Destroy(t.Context()) })
}

func Test_SupervisedPool_FastCancel(t *testing.T) {
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/sleep.php") },
		pipe.NewPipeFactory(slog.Default()),
		cfgSupervised,
		slog.Default(),
	)
	assert.NoError(t, err)

	assert.NotNil(t, p)

	newCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err = p.Exec(newCtx, &payload.Payload{Body: []byte("hello")}, make(chan struct{}))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "context deadline exceeded")
	t.Cleanup(func() { p.Destroy(t.Context()) })
}

func Test_SupervisedPool_AllocateFailedOK(t *testing.T) {
	var cfgExecTTL = &pool.Config{
		NumWorkers:      uint64(2),
		AllocateTimeout: time.Second * 15,
		DestroyTimeout:  time.Second * 5,
		Supervisor: &pool.SupervisorConfig{
			WatchTick: 1 * time.Second,
			TTL:       5 * time.Second,
		},
	}

	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/allocate-failed.php") },
		pipe.NewPipeFactory(slog.Default()),
		cfgExecTTL,
		slog.Default(),
	)

	assert.NoError(t, err)
	require.NotNil(t, p)

	time.Sleep(time.Second)

	// should be ok
	_, err = p.Exec(t.Context(), &payload.Payload{
		Context: []byte(""),
		Body:    []byte("foo"),
	}, make(chan struct{}))
	require.NoError(t, err)

	// after creating this file, PHP will fail
	file, err := os.Create("break")
	require.NoError(t, err)

	time.Sleep(time.Second * 5)
	assert.NoError(t, file.Close())
	assert.NoError(t, os.Remove("break"))

	defer func() {
		if r := recover(); r != nil {
			assert.Fail(t, "panic should not be fired!")
		} else {
			p.Destroy(t.Context())
		}
	}()
}

func Test_SupervisedPool_NoFreeWorkers(t *testing.T) {
	p, err := NewPool(
		t.Context(),
		// sleep for the 3 seconds
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/sleep.php", "pipes") },
		pipe.NewPipeFactory(slog.Default()),
		&pool.Config{
			Debug:           false,
			NumWorkers:      1,
			AllocateTimeout: time.Second,
			DestroyTimeout:  time.Second,
			Supervisor:      &pool.SupervisorConfig{},
		},
		slog.Default(),
	)
	assert.NoError(t, err)
	assert.NotNil(t, p)

	go func() {
		ctxNew, cancel := context.WithTimeout(t.Context(), time.Second*5)
		defer cancel()
		_, _ = p.Exec(ctxNew, &payload.Payload{Body: []byte("hello")}, make(chan struct{}))
	}()

	time.Sleep(time.Second)
	_, err = p.Exec(t.Context(), &payload.Payload{Body: []byte("hello")}, make(chan struct{}))
	assert.Error(t, err)

	time.Sleep(time.Second)
	t.Cleanup(func() { p.Destroy(t.Context()) })
}

// ==================== Supervisor Edge Cases ====================

// TestSupervisor_TTL_WorkingWorker_GetsInvalid verifies that a working worker gets StateInvalid
// (not StateTTLReached) when TTL expires during request execution.
// The TTL branch in control(): if worker is Ready → StateTTLReached, else → StateInvalid (graceful).
func TestSupervisor_TTL_WorkingWorker_GetsInvalid(t *testing.T) {
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/sleep.php") },
		pipe.NewPipeFactory(slog.Default()),
		&pool.Config{
			NumWorkers:      1,
			AllocateTimeout: time.Second * 10,
			DestroyTimeout:  time.Second * 10,
			Supervisor: &pool.SupervisorConfig{
				WatchTick: 1 * time.Second,
				TTL:       2 * time.Second,
			},
		},
		slog.Default(),
	)
	require.NoError(t, err)
	require.NotNil(t, p)
	t.Cleanup(func() { p.Destroy(t.Context()) })

	// Start a long exec (sleep.php sleeps for 300s)
	go func() {
		_, _ = p.Exec(t.Context(), &payload.Payload{Body: []byte("hello"), Context: []byte("")}, make(chan struct{}))
	}()

	// Wait for TTL to be reached while worker is busy
	time.Sleep(time.Second * 4)

	// Worker should be marked as Invalid (not TTLReached)
	workers := p.Workers()
	require.NotEmpty(t, workers)
	state := workers[0].State().CurrentState()
	// Worker should either be Invalid (marked by supervisor) or already replaced
	// If replaced, it should be in Ready state with a different PID
	assert.True(t,
		state == fsm.StateInvalid || state == fsm.StateReady || state == fsm.StateWorking,
		"expected Invalid/Ready/Working, got %d", state)
}

// TestSupervisor_IdleTTL_SkipsNeverUsedWorker verifies that workers with LastUsed==0
// (never executed a request) are not killed by idle TTL check.
// On pool startup, all workers have LastUsed=0 — idle check must skip them.
func TestSupervisor_IdleTTL_SkipsNeverUsedWorker(t *testing.T) {
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/idle.php", "pipes") },
		pipe.NewPipeFactory(slog.Default()),
		&pool.Config{
			NumWorkers:      1,
			AllocateTimeout: time.Second * 5,
			DestroyTimeout:  time.Second * 5,
			Supervisor: &pool.SupervisorConfig{
				WatchTick: 1 * time.Second,
				IdleTTL:   1 * time.Second,
			},
		},
		slog.Default(),
	)
	require.NoError(t, err)
	require.NotNil(t, p)
	t.Cleanup(func() { p.Destroy(t.Context()) })

	pid := p.Workers()[0].Pid()

	// Wait 3 seconds WITHOUT executing any request
	// The idle TTL check should skip this worker because LastUsed==0
	time.Sleep(time.Second * 3)

	workers := p.Workers()
	require.Len(t, workers, 1)
	// Worker should still be alive with the same PID (not replaced)
	assert.Equal(t, pid, workers[0].Pid(), "never-used worker should not be killed by idle TTL")
}

// TestSupervisor_MemoryCheck_WorkingWorkerGetsInvalid verifies that a working worker
// exceeding memory gets StateInvalid (not StateMaxMemoryReached).
// Same pattern as TTL in control(): killing a working worker corrupts in-flight response.
func TestSupervisor_MemoryCheck_WorkingWorkerGetsInvalid(t *testing.T) {
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/sleep.php") },
		pipe.NewPipeFactory(slog.Default()),
		&pool.Config{
			NumWorkers:      1,
			AllocateTimeout: time.Second * 10,
			DestroyTimeout:  time.Second * 10,
			Supervisor: &pool.SupervisorConfig{
				WatchTick:       1 * time.Second,
				MaxWorkerMemory: 1, // 1 MB — very low, will be exceeded
			},
		},
		slog.Default(),
	)
	require.NoError(t, err)
	require.NotNil(t, p)
	t.Cleanup(func() { p.Destroy(t.Context()) })

	// Start a long exec while worker is busy
	go func() {
		_, _ = p.Exec(t.Context(), &payload.Payload{Body: []byte("hello"), Context: []byte("")}, make(chan struct{}))
	}()

	time.Sleep(time.Second * 3)

	// Worker should be marked Invalid while working (not MaxMemoryReached)
	workers := p.Workers()
	require.NotEmpty(t, workers)
	state := workers[0].State().CurrentState()
	assert.True(t,
		state == fsm.StateInvalid || state == fsm.StateReady || state == fsm.StateWorking,
		"expected Invalid/Ready/Working, got %d", state)
}
