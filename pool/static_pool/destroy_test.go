package static_pool

import (
	"context"
	"log/slog"
	"os/exec"
	"testing"
	"time"

	"github.com/roadrunner-server/pool/v2/fsm"
	"github.com/roadrunner-server/pool/v2/ipc/pipe"
	"github.com/roadrunner-server/pool/v2/payload"
	"github.com/roadrunner-server/pool/v2/pool"
	"github.com/stretchr/testify/require"
)

// shutdownSlack covers the one-second ticker in WorkerWatcher plus process teardown.
const shutdownSlack = 3 * time.Second

// stuckWorkerPool returns a single-worker pool whose worker is parked in a request that never
// finishes, so shutdown can only end by hitting its timeout.
func stuckWorkerPool(t *testing.T, cfg *pool.Config) *Pool {
	t.Helper()

	p, err := NewPool(
		t.Context(),
		func(_ []string) *exec.Cmd { return exec.Command("php", "../../tests/sleep.php") },
		pipe.NewPipeFactory(slog.Default()),
		cfg,
		slog.Default(),
	)
	require.NoError(t, err)

	go func() {
		_, _ = p.Exec(context.Background(), &payload.Payload{Body: []byte("hello")}, make(chan struct{}))
	}()

	require.Eventually(t, func() bool {
		workers := p.Workers()
		return len(workers) == 1 && workers[0].State().Compare(fsm.StateWorking)
	}, 10*time.Second, 50*time.Millisecond, "worker never started the request")

	return p
}

func Test_Destroy_KillsStuckWorkerWithinBudget(t *testing.T) {
	const budget = time.Second

	for _, tc := range []struct {
		name           string
		destroyTimeout time.Duration
		ctxTimeout     time.Duration
	}{
		{name: "no caller deadline", destroyTimeout: budget},
		{name: "caller deadline is longer", destroyTimeout: budget, ctxTimeout: time.Minute},
		{name: "caller deadline is shorter", destroyTimeout: time.Minute, ctxTimeout: budget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := stuckWorkerPool(t, &pool.Config{
				NumWorkers:      1,
				AllocateTimeout: time.Minute,
				DestroyTimeout:  tc.destroyTimeout,
			})

			ctx := context.Background()
			if tc.ctxTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.ctxTimeout)
				defer cancel()
			}

			start := time.Now()
			p.Destroy(ctx)

			require.Less(t, time.Since(start), budget+shutdownSlack)
		})
	}
}

func Test_Reset_KillsStuckWorkerWithinBudget(t *testing.T) {
	const budget = time.Second

	p := stuckWorkerPool(t, &pool.Config{
		NumWorkers:      1,
		AllocateTimeout: time.Minute,
		DestroyTimeout:  time.Minute,
		ResetTimeout:    budget,
	})
	t.Cleanup(func() { p.Destroy(context.Background()) })

	start := time.Now()
	require.NoError(t, p.Reset(context.Background()))

	// Reset re-allocates the worker it just killed, so the bound is looser than Destroy's.
	require.Less(t, time.Since(start), budget+2*shutdownSlack)
}
