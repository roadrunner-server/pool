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
		IdleTimeout: 10,
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
				require.Equal(t, []byte("hello"), resp.Body())
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
