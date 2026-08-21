package static_pool

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
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

func childPids(t *testing.T) map[string]struct{} {
	t.Helper()
	out, _ := exec.Command("pgrep", "-P", strconv.Itoa(os.Getpid())).Output() //nolint:gosec
	pids := map[string]struct{}{}
	for p := range strings.FieldsSeq(string(out)) {
		pids[p] = struct{}{}
	}
	return pids
}

// newChildren returns child pids that appeared since the before snapshot.
func newChildren(t *testing.T, before map[string]struct{}) []string {
	t.Helper()
	var extra []string
	for p := range childPids(t) {
		if _, ok := before[p]; !ok {
			extra = append(extra, p)
		}
	}
	return extra
}

func TestExecDebug_ErrorDoesNotLeakWorker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pgrep is not available on windows")
	}
	before := childPids(t)
	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/client.php", "error", "pipes") },
		pipe.NewPipeFactory(slog.Default()),
		&pool.Config{
			Debug:           true,
			AllocateTimeout: time.Second * 5,
			DestroyTimeout:  time.Second * 5,
		},
		slog.Default(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { p.Destroy(t.Context()) })

	for range 3 {
		_, errE := p.Exec(t.Context(), &payload.Payload{Body: []byte("boom")}, make(chan struct{}))
		require.Error(t, errE)
	}

	assert.Eventually(t, func() bool {
		return len(newChildren(t, before)) == 0
	}, time.Second*5, time.Millisecond*250, "debug workers must not survive failed execs")
}
