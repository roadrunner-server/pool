package static_pool

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/roadrunner-server/pool/v2/pool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// white-box test: addMoreWorkers has no in-pool trigger anymore (it used to be fired
// by the Exec NoFreeWorkers path), the scale-up signal is expected to come from plugins
func Test_DynAllocator_ScaleUpAndIdleDealloc(t *testing.T) {
	cfg := &pool.Config{
		NumWorkers:      1,
		AllocateTimeout: time.Second * 10,
		DestroyTimeout:  time.Second * 10,
		DynamicAllocatorOpts: &pool.DynamicAllocationOpts{
			MaxWorkers:  5,
			SpawnRate:   2,
			IdleTimeout: time.Second * 2,
		},
	}

	p, err := NewPool(t.Context(), sleepCmd, cfg, slog.Default())
	require.NoError(t, err)
	require.NotNil(t, p.dynamicAllocator)

	p.dynamicAllocator.addMoreWorkers()
	assert.Equal(t, uint64(2), p.NumDynamic())
	assert.Len(t, p.Workers(), 3)

	// an immediate second call is rate-limited (1s cooldown) and must not allocate
	p.dynamicAllocator.addMoreWorkers()
	assert.Equal(t, uint64(2), p.NumDynamic())

	// after the idle window, the dynamically allocated workers are deallocated
	require.Eventually(t, func() bool {
		return p.NumDynamic() == 0 && len(p.Workers()) == 1
	}, 15*time.Second, 100*time.Millisecond)

	p.Destroy(context.Background())
}

func Test_DynAllocator_MaxWorkersCap(t *testing.T) {
	cfg := &pool.Config{
		NumWorkers:      1,
		AllocateTimeout: time.Second * 10,
		DestroyTimeout:  time.Second * 10,
		DynamicAllocatorOpts: &pool.DynamicAllocationOpts{
			MaxWorkers:  3,
			SpawnRate:   2,
			IdleTimeout: time.Second * 30,
		},
	}

	p, err := NewPool(t.Context(), sleepCmd, cfg, slog.Default())
	require.NoError(t, err)

	p.dynamicAllocator.addMoreWorkers()
	assert.Equal(t, uint64(2), p.NumDynamic())

	// wait out the rate-limiter cooldown; the next batch is capped at max_workers
	require.Eventually(t, func() bool {
		p.dynamicAllocator.addMoreWorkers()
		return p.NumDynamic() == 3
	}, 10*time.Second, 200*time.Millisecond)

	assert.Len(t, p.Workers(), 4)

	p.Destroy(context.Background())
}
