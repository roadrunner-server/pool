package static_pool

import (
	"os/exec"
	"testing"
	"time"

	"log/slog"

	"github.com/roadrunner-server/pool/v2/ipc/pipe"
	"github.com/roadrunner-server/pool/v2/payload"
	"github.com/roadrunner-server/pool/v2/pool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecDebug_NonStream(t *testing.T) {
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/client.php", "echo", "pipes") },
		pipe.NewPipeFactory(slog.Default()),
		&pool.Config{
			Debug:           true,
			AllocateTimeout: time.Second * 5,
			DestroyTimeout:  time.Second * 5,
		},
		slog.Default(),
	)
	require.NoError(t, err)
	require.NotNil(t, p)
	t.Cleanup(func() { p.Destroy(t.Context()) })

	// Debug mode should have zero pre-allocated workers
	assert.Empty(t, p.Workers())

	// Execute request — goes through execDebug (non-stream branch)
	r, err := p.Exec(t.Context(), &payload.Payload{Body: []byte("hello"), Context: []byte("")}, make(chan struct{}))
	require.NoError(t, err)

	resp := <-r
	assert.Equal(t, []byte("hello"), resp.Body())
	assert.NoError(t, resp.Error())

	// Worker should be destroyed after response — no workers in pool
	assert.Empty(t, p.Workers())
}

func TestExecDebug_FreshWorkerPerRequest(t *testing.T) {
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/client.php", "pid", "pipes") },
		pipe.NewPipeFactory(slog.Default()),
		&pool.Config{
			Debug:           true,
			AllocateTimeout: time.Second * 5,
			DestroyTimeout:  time.Second * 5,
		},
		slog.Default(),
	)
	require.NoError(t, err)
	require.NotNil(t, p)
	t.Cleanup(func() { p.Destroy(t.Context()) })

	// Each request in debug mode should use a fresh worker (different PID)
	pids := make(map[string]struct{})
	for range 3 {
		r, err := p.Exec(t.Context(), &payload.Payload{Body: []byte("hello")}, make(chan struct{}))
		require.NoError(t, err)
		resp := <-r
		pids[string(resp.Body())] = struct{}{}
	}

	// All PIDs should be different — each request creates a new worker
	assert.Len(t, pids, 3, "debug mode should spawn a fresh worker for each request")
}
