package static_pool

import (
	"os/exec"
	"sync"
	"testing"
	"time"

	"log/slog"

	"github.com/roadrunner-server/pool/v2/ipc/pipe"
	"github.com/roadrunner-server/pool/v2/payload"
	"github.com/roadrunner-server/pool/v2/pool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testDynCfg = &pool.Config{
	NumWorkers:      5,
	AllocateTimeout: time.Second * 5,
	DestroyTimeout:  time.Second * 10,
	DynamicAllocatorOpts: &pool.DynamicAllocationOpts{
		MaxWorkers:  25,
		SpawnRate:   5,
		IdleTimeout: time.Second * 10,
	},
}

func Test_DynAllocator(t *testing.T) {
	np, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/client.php", "echo", "pipes") },
		pipe.NewPipeFactory(slog.Default()),
		testDynCfg,
		slog.Default(),
	)
	assert.NoError(t, err)
	assert.NotNil(t, np)

	r, err := np.Exec(t.Context(), &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
	require.NoError(t, err)
	resp := <-r

	assert.Equal(t, []byte("hello"), resp.Body())
	assert.NoError(t, err)
	t.Cleanup(func() { np.Destroy(t.Context()) })
}

func Test_DynAllocatorManyReq(t *testing.T) {
	var testDynCfgMany = &pool.Config{
		NumWorkers:      5,
		MaxJobs:         2,
		AllocateTimeout: time.Second * 1,
		DestroyTimeout:  time.Second * 10,
		ResetTimeout:    time.Second * 5,
		DynamicAllocatorOpts: &pool.DynamicAllocationOpts{
			MaxWorkers:  25,
			SpawnRate:   5,
			IdleTimeout: time.Second * 10,
		},
	}

	np, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd {
			return exec.Command("php", "../../tests/client.php", "slow_req", "pipes")
		},
		pipe.NewPipeFactory(slog.Default()),
		testDynCfgMany,
		slog.Default(),
	)
	assert.NoError(t, err)
	assert.NotNil(t, np)

	wg := &sync.WaitGroup{}
	go func() {
		for range 1000 {
			wg.Go(func() {
				r, erre := np.Exec(t.Context(), &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
				if erre != nil {
					t.Log("failed request: ", erre.Error())
					return
				}
				resp := <-r
				assert.Equal(t, []byte("hello"), resp.Body())
			})
		}
	}()

	go func() {
		for range 10 {
			time.Sleep(time.Second)
			_ = np.Reset(t.Context())
		}
	}()

	wg.Wait()

	time.Sleep(time.Second * 30)

	assert.Equal(t, 5, len(np.Workers()))
	t.Cleanup(func() { np.Destroy(t.Context()) })
}

func Test_DynamicPool_OverMax(t *testing.T) {
	dynAllCfg := &pool.Config{
		NumWorkers:      1,
		AllocateTimeout: time.Second * 5,
		DestroyTimeout:  time.Second * 20,
		DynamicAllocatorOpts: &pool.DynamicAllocationOpts{
			MaxWorkers:  1,
			IdleTimeout: time.Second * 15,
			SpawnRate:   10,
		},
	}

	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd {
			return exec.Command("php", "../../tests/worker-slow-dyn.php")
		},
		pipe.NewPipeFactory(slog.Default()),
		dynAllCfg,
		slog.Default(),
	)
	assert.NoError(t, err)
	assert.NotNil(t, p)

	wg := &sync.WaitGroup{}

	wg.Go(func() {
		t.Log("sending request 1")
		r, err := p.Exec(t.Context(), &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
		require.NoError(t, err)
		select {
		case resp := <-r:
			assert.Equal(t, []byte("hello world"), resp.Body())
			assert.NoError(t, err)
			t.Log("request 1 finished")
		case <-time.After(time.Second * 10):
			assert.Fail(t, "timeout")
		}
	})
	wg.Go(func() {
		// sleep to ensure the first request is being processed first
		// this request should trigger dynamic allocation attempt and return an error
		time.Sleep(time.Second)
		t.Log("sending request 2")
		_, err := p.Exec(t.Context(), &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
		require.Error(t, err)
	})

	t.Log("waiting for the requests 1 and 2")
	wg.Wait()
	t.Log("wait 1 and 2 finished")

	// request 3 and 4 should be processed normally, since we have 2 workers now (1 initial + 1 dynamic)
	t.Log("starting requests 3 and 4")
	require.Len(t, p.Workers(), 2)

	wg.Go(func() {
		t.Log("request 3")
		r, err := p.Exec(t.Context(), &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
		require.NoError(t, err)
		select {
		case resp := <-r:
			t.Log("request 3 finished")
			assert.Equal(t, []byte("hello world"), resp.Body())
			assert.NoError(t, err)
		case <-time.After(time.Second * 10):
			assert.Fail(t, "timeout")
		}
	})
	wg.Go(func() {
		t.Log("request 4")
		r, err := p.Exec(t.Context(), &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
		require.NoError(t, err)
		select {
		case resp := <-r:
			t.Log("request 4 finished")
			assert.Equal(t, []byte("hello world"), resp.Body())
			assert.NoError(t, err)
		case <-time.After(time.Second * 10):
			assert.Fail(t, "timeout")
		}
	})

	t.Log("waiting for the requests 3 and 4")
	wg.Wait()
	time.Sleep(time.Second * 20)
	assert.Len(t, p.Workers(), 1)
	t.Cleanup(func() {
		p.Destroy(t.Context())
	})
}

func Test_DynamicPool(t *testing.T) {
	dynAllCfg := &pool.Config{
		NumWorkers:      1,
		AllocateTimeout: time.Second * 5,
		DestroyTimeout:  time.Second,
		DynamicAllocatorOpts: &pool.DynamicAllocationOpts{
			MaxWorkers:  5,
			IdleTimeout: time.Second * 15,
			SpawnRate:   2,
		},
	}

	p, errp := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd {
			return exec.Command("php", "../../tests/worker-slow-dyn.php")
		},
		pipe.NewPipeFactory(slog.Default()),
		dynAllCfg,
		slog.Default(),
	)
	assert.NoError(t, errp)
	assert.NotNil(t, p)

	wg := &sync.WaitGroup{}
	wg.Go(func() {
		r, err := p.Exec(t.Context(), &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
		require.NoError(t, err)
		select {
		case resp := <-r:
			assert.Equal(t, []byte("hello world"), resp.Body())
			assert.NoError(t, err)
		case <-time.After(time.Second * 10):
			assert.Fail(t, "timeout")
		}
	})

	wg.Go(func() {
		time.Sleep(time.Second)
		_, err := p.Exec(t.Context(), &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
		require.Error(t, err)
	})

	wg.Wait()

	time.Sleep(time.Second * 20)
	require.Len(t, p.Workers(), 1)
	t.Cleanup(func() {
		p.Destroy(t.Context())
	})
}

func Test_DynamicPool_500W(t *testing.T) {
	dynAllCfg := &pool.Config{
		NumWorkers:      1,
		AllocateTimeout: time.Second * 5,
		DestroyTimeout:  time.Second,
		DynamicAllocatorOpts: &pool.DynamicAllocationOpts{
			MaxWorkers:  10,
			IdleTimeout: time.Second * 15,
			// should be corrected to 10 by RR
			SpawnRate: 11,
		},
	}

	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd {
			return exec.Command("php", "../../tests/worker-slow-dyn.php")
		},
		pipe.NewPipeFactory(slog.Default()),
		dynAllCfg,
		slog.Default(),
	)
	assert.NoError(t, err)
	assert.NotNil(t, p)
	require.Len(t, p.Workers(), 1)

	wg := &sync.WaitGroup{}

	wg.Go(func() {
		r, err := p.Exec(t.Context(), &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
		assert.NoError(t, err)
		select {
		case resp := <-r:
			assert.Equal(t, []byte("hello world"), resp.Body())
			assert.NoError(t, err)
		case <-time.After(time.Second * 10):
			assert.Fail(t, "timeout")
		}
	})

	wg.Go(func() {
		time.Sleep(time.Second * 1)
		_, err := p.Exec(t.Context(), &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
		require.Error(t, err)
	})

	wg.Wait()

	time.Sleep(time.Second * 30)

	require.Len(t, p.Workers(), 1)
	t.Cleanup(func() { p.Destroy(t.Context()) })
}

// Test_DynAllocator_100Workers verifies that the dynamic allocator can scale up to 100 dynamic workers
// in batches of 20 (spawnRate) and then properly deallocate them all after the idle timeout.
func Test_DynAllocator_100Workers(t *testing.T) {
	cfg := &pool.Config{
		NumWorkers:      5,
		AllocateTimeout: time.Second,
		DestroyTimeout:  time.Second * 30,
		DynamicAllocatorOpts: &pool.DynamicAllocationOpts{
			MaxWorkers:  100,
			SpawnRate:   20,
			IdleTimeout: time.Second * 3,
		},
	}

	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd {
			return exec.Command("php", "../../tests/client.php", "delay", "pipes")
		},
		pipe.NewPipeFactory(slog.Default()),
		cfg,
		slog.Default(),
	)
	require.NoError(t, err)
	require.NotNil(t, p)
	t.Cleanup(func() { p.Destroy(t.Context()) })
	require.Len(t, p.Workers(), 5)

	wg := &sync.WaitGroup{}
	// Fire requests in waves to sustain pressure across rate limiter windows.
	// Each wave saturates current workers; failed requests trigger addMoreWorkers (20 per call, rate-limited to 1/sec).
	// Over 8 waves ≈ 8 seconds: up to 5 batches * 20 = 100 dynamic workers.
	for wave := range 8 {
		for range 60 {
			wg.Go(func() {
				// 5-second delay keeps workers busy long enough to create sustained pressure
				r, erre := p.Exec(t.Context(), &payload.Payload{Body: []byte("5000"), Context: nil}, make(chan struct{}))
				if erre != nil {
					return
				}
				<-r
			})
		}
		if wave < 7 {
			time.Sleep(time.Second)
		}
	}

	wg.Wait()

	totalAfterLoad := len(p.Workers())
	dynAfterLoad := p.NumDynamic()
	t.Log("workers after load:", totalAfterLoad, "dynamic:", dynAfterLoad)
	assert.Greater(t, dynAfterLoad, uint64(0), "dynamic allocation should have occurred")

	// Wait for idle timeout (3s) + deallocation cycles.
	// With 100 dynamic workers and SpawnRate=20: 5 deallocation batches, each triggered at IdleTimeout interval.
	// 5 batches * 3s = 15s + extra buffer.
	time.Sleep(time.Second * 30)

	assert.Equal(t, uint64(0), p.NumDynamic(), "all dynamic workers should be deallocated")
	assert.Len(t, p.Workers(), 5, "should return to base worker count")
}

// Test_DynAllocator_ReallocationCycle verifies that after all dynamic workers are deallocated
// (idle TTL listener exits, started=false), a new pressure spike correctly restarts the listener
// and allocates workers again. This tests the full listener lifecycle: start → deallocate → stop → restart.
func Test_DynAllocator_ReallocationCycle(t *testing.T) {
	cfg := &pool.Config{
		NumWorkers:      2,
		AllocateTimeout: time.Second,
		DestroyTimeout:  time.Second * 20,
		DynamicAllocatorOpts: &pool.DynamicAllocationOpts{
			MaxWorkers:  10,
			SpawnRate:   5,
			IdleTimeout: time.Second * 2,
		},
	}

	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd {
			return exec.Command("php", "../../tests/client.php", "delay", "pipes")
		},
		pipe.NewPipeFactory(slog.Default()),
		cfg,
		slog.Default(),
	)
	require.NoError(t, err)
	require.NotNil(t, p)
	t.Cleanup(func() { p.Destroy(t.Context()) })
	require.Len(t, p.Workers(), 2)

	// === Cycle 1: Trigger allocation, then wait for full deallocation ===
	wg := &sync.WaitGroup{}
	for range 30 {
		wg.Go(func() {
			r, erre := p.Exec(t.Context(), &payload.Payload{Body: []byte("3000"), Context: nil}, make(chan struct{}))
			if erre != nil {
				return
			}
			<-r
		})
	}
	wg.Wait()

	dyn1 := p.NumDynamic()
	t.Log("cycle 1 - dynamic workers allocated:", dyn1)
	assert.Greater(t, dyn1, uint64(0), "cycle 1: dynamic allocation should have occurred")

	// Wait for full deallocation: idle timeout (2s) + deallocation batches (2 batches * 2s = 4s) + buffer
	time.Sleep(time.Second * 10)

	assert.Equal(t, uint64(0), p.NumDynamic(), "cycle 1: all dynamic workers should be deallocated")
	assert.Len(t, p.Workers(), 2, "cycle 1: should return to base count")

	// === Cycle 2: Re-trigger allocation (listener must restart from started=false) ===
	for range 30 {
		wg.Go(func() {
			r, erre := p.Exec(t.Context(), &payload.Payload{Body: []byte("3000"), Context: nil}, make(chan struct{}))
			if erre != nil {
				return
			}
			<-r
		})
	}
	wg.Wait()

	dyn2 := p.NumDynamic()
	t.Log("cycle 2 - dynamic workers allocated:", dyn2)
	assert.Greater(t, dyn2, uint64(0), "cycle 2: dynamic allocation should have occurred (listener restarted)")

	// Wait for full deallocation again
	time.Sleep(time.Second * 10)

	assert.Equal(t, uint64(0), p.NumDynamic(), "cycle 2: all dynamic workers should be deallocated")
	assert.Len(t, p.Workers(), 2, "cycle 2: should return to base count after re-allocation")
}

// ==================== Phase 6: Dynamic Allocator Edge Cases ====================

// Test_DynAllocator_SpawnRate_CappedByMaxWorkers verifies that spawnRate doesn't exceed maxWorkers.
// Inner loop break on line 104 (`currAllocated >= maxWorkers`). Without it, over-allocation occurs.
func Test_DynAllocator_SpawnRate_CappedByMaxWorkers(t *testing.T) {
	cfg := &pool.Config{
		NumWorkers:      1,
		AllocateTimeout: time.Second * 5,
		DestroyTimeout:  time.Second * 20,
		DynamicAllocatorOpts: &pool.DynamicAllocationOpts{
			MaxWorkers:  3,
			SpawnRate:   10, // higher than maxWorkers
			IdleTimeout: time.Second * 10,
		},
	}

	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd {
			return exec.Command("php", "../../tests/worker-slow-dyn.php")
		},
		pipe.NewPipeFactory(slog.Default()),
		cfg,
		slog.Default(),
	)
	require.NoError(t, err)
	require.NotNil(t, p)
	t.Cleanup(func() { p.Destroy(t.Context()) })

	wg := &sync.WaitGroup{}
	// Fire enough requests to trigger dynamic allocation
	for range 20 {
		wg.Go(func() {
			r, erre := p.Exec(t.Context(), &payload.Payload{Body: []byte("hello")}, make(chan struct{}))
			if erre != nil {
				return
			}
			<-r
		})
	}

	// Wait a bit for allocation to happen
	time.Sleep(time.Second * 3)

	// Dynamic workers should not exceed maxWorkers=3
	dynCount := p.NumDynamic()
	assert.LessOrEqual(t, dynCount, uint64(3),
		"dynamic workers should not exceed maxWorkers, got %d", dynCount)

	wg.Wait()
}

// Test_DynAllocator_CounterConsistency_AfterFailedRemoval verifies that currAllocated
// doesn't decrement when RemoveWorker fails (all workers busy).
// Line 181-182: break on removal failure prevents counter desync.
func Test_DynAllocator_CounterConsistency_AfterFailedRemoval(t *testing.T) {
	cfg := &pool.Config{
		NumWorkers:      1,
		AllocateTimeout: time.Second * 5,
		DestroyTimeout:  time.Second * 30,
		DynamicAllocatorOpts: &pool.DynamicAllocationOpts{
			MaxWorkers:  5,
			SpawnRate:   5,
			IdleTimeout: time.Second * 3,
		},
	}

	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd {
			return exec.Command("php", "../../tests/client.php", "delay", "pipes")
		},
		pipe.NewPipeFactory(slog.Default()),
		cfg,
		slog.Default(),
	)
	require.NoError(t, err)
	require.NotNil(t, p)
	t.Cleanup(func() { p.Destroy(t.Context()) })

	wg := &sync.WaitGroup{}
	// Fire requests to trigger dynamic allocation and keep workers busy
	for range 20 {
		wg.Go(func() {
			// 3s delay keeps workers busy during deallocation attempt
			r, erre := p.Exec(t.Context(), &payload.Payload{Body: []byte("3000")}, make(chan struct{}))
			if erre != nil {
				return
			}
			<-r
		})
	}

	time.Sleep(time.Second * 2)

	// Verify dynamic workers were allocated
	dynBefore := p.NumDynamic()
	t.Log("dynamic workers before deallocation:", dynBefore)

	wg.Wait()

	// After all requests complete, wait for deallocation
	time.Sleep(time.Second * 10)

	// Counter should eventually reach 0 (all dynamic workers deallocated)
	assert.Equal(t, uint64(0), p.NumDynamic(),
		"all dynamic workers should be deallocated after idle timeout")
}

// Test_DynAllocator_RateLimit_ThunderingHerd verifies that the rate limiter prevents
// over-allocation under thundering herd conditions.
// Rate limiter (line 70) gates concurrent allocation. Without it, each pending request
// could trigger a spawn batch.
func Test_DynAllocator_RateLimit_ThunderingHerd(t *testing.T) {
	cfg := &pool.Config{
		NumWorkers:      1,
		AllocateTimeout: time.Second * 5,
		DestroyTimeout:  time.Second * 30,
		DynamicAllocatorOpts: &pool.DynamicAllocationOpts{
			MaxWorkers:  5,
			SpawnRate:   5,
			IdleTimeout: time.Second * 10,
		},
	}

	p, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd {
			return exec.Command("php", "../../tests/worker-slow-dyn.php")
		},
		pipe.NewPipeFactory(slog.Default()),
		cfg,
		slog.Default(),
	)
	require.NoError(t, err)
	require.NotNil(t, p)
	t.Cleanup(func() { p.Destroy(t.Context()) })

	wg := &sync.WaitGroup{}
	// Fire 50 simultaneous requests — thundering herd
	for range 50 {
		wg.Go(func() {
			r, erre := p.Exec(t.Context(), &payload.Payload{Body: []byte("hello")}, make(chan struct{}))
			if erre != nil {
				return
			}
			<-r
		})
	}

	// Wait for allocation attempts
	time.Sleep(time.Second * 3)

	// Dynamic workers should not exceed maxWorkers despite 50 concurrent requests
	totalWorkers := len(p.Workers())
	dynWorkers := p.NumDynamic()

	t.Log("total workers:", totalWorkers, "dynamic:", dynWorkers)
	// 1 base + up to 5 dynamic = max 6 total
	assert.LessOrEqual(t, totalWorkers, 6,
		"total workers should not exceed base + maxDynamic, got %d", totalWorkers)
	assert.LessOrEqual(t, dynWorkers, uint64(5),
		"dynamic workers should not exceed maxWorkers=5, got %d", dynWorkers)

	wg.Wait()
}
