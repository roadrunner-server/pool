package static_pool

import (
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/roadrunner-server/pool/ipc/pipe"
	"github.com/roadrunner-server/pool/payload"
	"github.com/roadrunner-server/pool/pool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

var dynlog = func() *zap.Logger {
	logger, _ := zap.NewDevelopment()
	return logger
}

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
	ctx := context.Background()
	np, err := NewPool(
		ctx,
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/client.php", "echo", "pipes") },
		pipe.NewPipeFactory(dynlog()),
		testDynCfg,
		dynlog(),
	)
	assert.NoError(t, err)
	assert.NotNil(t, np)

	r, err := np.Exec(ctx, &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
	require.NoError(t, err)
	resp := <-r

	assert.Equal(t, []byte("hello"), resp.Body())
	assert.NoError(t, err)

	np.Destroy(ctx)
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

	ctx := context.Background()
	np, err := NewPool(
		ctx,
		func(cmd []string) *exec.Cmd {
			return exec.Command("php", "../../tests/client.php", "slow_req", "pipes")
		},
		pipe.NewPipeFactory(dynlog()),
		testDynCfgMany,
		dynlog(),
	)
	assert.NoError(t, err)
	assert.NotNil(t, np)

	wg := &sync.WaitGroup{}
	wg.Add(1000)
	go func() {
		for range 1000 {
			go func() {
				defer wg.Done()
				r, erre := np.Exec(ctx, &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
				if erre != nil {
					t.Log("failed request: ", erre.Error())
					return
				}
				resp := <-r
				assert.Equal(t, []byte("hello"), resp.Body())
			}()
		}
	}()

	go func() {
		for range 10 {
			time.Sleep(time.Second)
			_ = np.Reset(context.Background())
		}
	}()

	wg.Wait()

	time.Sleep(time.Second * 30)

	assert.Equal(t, 5, len(np.Workers()))

	np.Destroy(ctx)
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

	ctx := context.Background()
	p, err := NewPool(
		ctx,
		func(cmd []string) *exec.Cmd {
			return exec.Command("php", "../../tests/worker-slow-dyn.php")
		},
		pipe.NewPipeFactory(log()),
		dynAllCfg,
		log(),
	)
	assert.NoError(t, err)
	assert.NotNil(t, p)

	wg := &sync.WaitGroup{}
	wg.Add(2)

	go func() {
		t.Log("sending request 1")
		r, err := p.Exec(ctx, &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
		require.NoError(t, err)
		select {
		case resp := <-r:
			assert.Equal(t, []byte("hello world"), resp.Body())
			assert.NoError(t, err)
			t.Log("request 1 finished")
		case <-time.After(time.Second * 10):
			assert.Fail(t, "timeout")
		}

		wg.Done()
	}()
	go func() {
		// sleep to ensure the first request is being processed first
		// this request should trigger dynamic allocation attempt and return an error
		time.Sleep(time.Second)
		t.Log("sending request 2")
		_, err := p.Exec(ctx, &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
		require.Error(t, err)
		wg.Done()
	}()

	t.Log("waiting for the requests 1 and 2")
	wg.Wait()
	t.Log("wait 1 and 2 finished")

	wg.Add(2)
	// request 3 and 4 should be processed normally, since we have 2 workers now (1 initial + 1 dynamic)
	t.Log("starting requests 3 and 4")
	require.Len(t, p.Workers(), 2)

	go func() {
		t.Log("request 3")
		r, err := p.Exec(ctx, &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
		require.NoError(t, err)
		select {
		case resp := <-r:
			t.Log("request 3 finished")
			assert.Equal(t, []byte("hello world"), resp.Body())
			assert.NoError(t, err)
		case <-time.After(time.Second * 10):
			assert.Fail(t, "timeout")
		}

		wg.Done()
	}()
	go func() {
		t.Log("request 4")
		r, err := p.Exec(ctx, &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
		require.NoError(t, err)
		select {
		case resp := <-r:
			t.Log("request 4 finished")
			assert.Equal(t, []byte("hello world"), resp.Body())
			assert.NoError(t, err)
		case <-time.After(time.Second * 10):
			assert.Fail(t, "timeout")
		}

		wg.Done()
	}()

	t.Log("waiting for the requests 3 and 4")
	wg.Wait()
	time.Sleep(time.Second * 20)
	assert.Len(t, p.Workers(), 1)
	p.Destroy(ctx)
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

	ctx := context.Background()
	p, err := NewPool(
		ctx,
		func(cmd []string) *exec.Cmd {
			return exec.Command("php", "../../tests/worker-slow-dyn.php")
		},
		pipe.NewPipeFactory(log()),
		dynAllCfg,
		log(),
	)
	assert.NoError(t, err)
	assert.NotNil(t, p)

	wg := &sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		r, err := p.Exec(ctx, &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
		require.NoError(t, err)
		select {
		case resp := <-r:
			assert.Equal(t, []byte("hello world"), resp.Body())
			assert.NoError(t, err)
		case <-time.After(time.Second * 10):
			assert.Fail(t, "timeout")
		}
	}()

	go func() {
		time.Sleep(time.Second)
		_, err := p.Exec(ctx, &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
		require.Error(t, err)
		wg.Done()
	}()

	wg.Wait()

	time.Sleep(time.Second * 20)
	require.Len(t, p.Workers(), 1)

	p.Destroy(ctx)
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

	ctx := context.Background()
	p, err := NewPool(
		ctx,
		func(cmd []string) *exec.Cmd {
			return exec.Command("php", "../../tests/worker-slow-dyn.php")
		},
		pipe.NewPipeFactory(log()),
		dynAllCfg,
		log(),
	)
	assert.NoError(t, err)
	assert.NotNil(t, p)
	require.Len(t, p.Workers(), 1)

	wg := &sync.WaitGroup{}
	wg.Add(2)

	go func() {
		r, err := p.Exec(ctx, &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
		assert.NoError(t, err)
		select {
		case resp := <-r:
			assert.Equal(t, []byte("hello world"), resp.Body())
			assert.NoError(t, err)
		case <-time.After(time.Second * 10):
			assert.Fail(t, "timeout")
		}

		wg.Done()
	}()

	go func() {
		time.Sleep(time.Second * 1)
		_, err := p.Exec(ctx, &payload.Payload{Body: []byte("hello"), Context: nil}, make(chan struct{}))
		require.Error(t, err)
		wg.Done()
	}()

	wg.Wait()

	time.Sleep(time.Second * 30)

	require.Len(t, p.Workers(), 1)
	p.Destroy(ctx)
}
