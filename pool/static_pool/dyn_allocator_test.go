package static_pool

import (
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
	np, err := NewPool(
		t.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/client.php", "echo", "pipes") },
		pipe.NewPipeFactory(dynlog()),
		testDynCfg,
		dynlog(),
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
		pipe.NewPipeFactory(dynlog()),
		testDynCfgMany,
		dynlog(),
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
		pipe.NewPipeFactory(log()),
		dynAllCfg,
		log(),
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
		pipe.NewPipeFactory(log()),
		dynAllCfg,
		log(),
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
		pipe.NewPipeFactory(log()),
		dynAllCfg,
		log(),
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
