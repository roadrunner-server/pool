package pipe

import (
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/pool/fsm"
	"github.com/roadrunner-server/pool/payload"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

var log = zap.NewNop()

func Test_GetState2(t *testing.T) {
	cmd := exec.Command("php", "../../tests/client.php", "echo", "pipes")

	w, err := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)
	go func() {
		assert.NoError(t, w.Wait())
		assert.Equal(t, fsm.StateStopped, w.State().CurrentState())
	}()

	assert.NoError(t, err)
	assert.NotNil(t, w)

	assert.Equal(t, fsm.StateReady, w.State().CurrentState())
	assert.NoError(t, w.Stop())
}

func Test_Kill2(t *testing.T) {
	cmd := exec.Command("php", "../../tests/client.php", "echo", "pipes")

	w, err := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		assert.Error(t, w.Wait())
		assert.Equal(t, fsm.StateErrored, w.State().CurrentState())
	}()

	assert.NoError(t, err)
	assert.NotNil(t, w)

	assert.Equal(t, fsm.StateReady, w.State().CurrentState())
	err = w.Kill()
	if err != nil {
		t.Errorf("error killing the Process: error %v", err)
	}
	wg.Wait()
}

func Test_Pipe_Start2(t *testing.T) {
	cmd := exec.Command("php", "../../tests/client.php", "echo", "pipes")

	w, err := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)
	assert.NoError(t, err)
	assert.NotNil(t, w)

	go func() {
		assert.NoError(t, w.Wait())
	}()

	assert.NoError(t, w.Stop())
}

func Test_Pipe_StartError2(t *testing.T) {
	cmd := exec.Command("php", "../../tests/client.php", "echo", "pipes")
	err := cmd.Start()
	if err != nil {
		t.Errorf("error running the command: error %v", err)
	}

	w, err := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)
	assert.Error(t, err)
	assert.Nil(t, w)
}

func Test_Pipe_PipeError3(t *testing.T) {
	cmd := exec.Command("php", "../../tests/client.php", "echo", "pipes")
	_, err := cmd.StdinPipe()
	if err != nil {
		t.Errorf("error creating the STDIN pipe: error %v", err)
	}

	w, err := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)
	assert.Error(t, err)
	assert.Nil(t, w)
}

func Test_Pipe_PipeError4(t *testing.T) {
	cmd := exec.Command("php", "../../tests/client.php", "echo", "pipes")
	_, err := cmd.StdinPipe()
	if err != nil {
		t.Errorf("error creating the STDIN pipe: error %v", err)
	}

	w, err := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)
	assert.Error(t, err)
	assert.Nil(t, w)
}

func Test_Pipe_Failboot2(t *testing.T) {
	cmd := exec.Command("php", "../../tests/failboot.php")
	w, err := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)
	assert.Nil(t, w)
	assert.Error(t, err)
}

func Test_Pipe_Invalid2(t *testing.T) {
	cmd := exec.Command("php", "../../tests/invalid.php")
	w, err := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)
	assert.Error(t, err)
	assert.Nil(t, w)
}

func Test_Pipe_Echo2(t *testing.T) {
	cmd := exec.Command("php", "../../tests/client.php", "echo", "pipes")
	w, err := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)
	assert.NoError(t, err)

	res, err := w.Exec(context.Background(), &payload.Payload{Body: []byte("hello")})

	assert.NoError(t, err)
	assert.NotNil(t, res)
	assert.NotNil(t, res.Body)
	assert.Empty(t, res.Context)

	go func() {
		if w.Wait() != nil {
			t.Fail()
		}
	}()

	assert.Equal(t, "hello", res.String())
	err = w.Stop()
	assert.NoError(t, err)
}

func Test_Pipe_Broken2(t *testing.T) {
	cmd := exec.Command("php", "../../tests/client.php", "broken", "pipes")
	w, err := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)
	assert.NoError(t, err)
	require.NotNil(t, w)

	res, err := w.Exec(context.Background(), &payload.Payload{Body: []byte("hello")})
	assert.Error(t, err)
	assert.Nil(t, res)

	time.Sleep(time.Second)
	err = w.Stop()
	assert.Error(t, err)
}

func Benchmark_Pipe_SpawnWorker_Stop2(b *testing.B) {
	f := NewPipeFactory(log)
	for b.Loop() {
		cmd := exec.Command("php", "../../tests/client.php", "echo", "pipes")
		w, _ := f.SpawnWorkerWithContext(context.Background(), cmd)
		go func() {
			if w.Wait() != nil {
				b.Fail()
			}
		}()

		err := w.Stop()
		if err != nil {
			b.Errorf("error stopping the worker: error %v", err)
		}
	}
}

func Benchmark_Pipe_Worker_ExecEcho2(b *testing.B) {
	cmd := exec.Command("php", "../../tests/client.php", "echo", "pipes")

	w, _ := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)

	b.ReportAllocs()

	go func() {
		err := w.Wait()
		if err != nil {
			b.Errorf("error waiting the worker: error %v", err)
		}
	}()
	defer func() {
		err := w.Stop()
		if err != nil {
			b.Errorf("error stopping the worker: error %v", err)
		}
	}()

	for b.Loop() {
		if _, err := w.Exec(context.Background(), &payload.Payload{Body: []byte("hello")}); err != nil {
			b.Fail()
		}
	}
}

func Benchmark_Pipe_Worker_ExecEcho4(b *testing.B) {
	cmd := exec.Command("php", "../../tests/client.php", "echo", "pipes")
	w, err := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)
	if err != nil {
		b.Fatal(err)
	}

	defer func() {
		err = w.Stop()
		if err != nil {
			b.Errorf("error stopping the Process: error %v", err)
		}
	}()

	for b.Loop() {
		if _, err := w.Exec(context.Background(), &payload.Payload{Body: []byte("hello")}); err != nil {
			b.Fail()
		}
	}
}

func Benchmark_Pipe_Worker_ExecEchoWithoutContext2(b *testing.B) {
	cmd := exec.Command("php", "../../tests/client.php", "echo", "pipes")
	w, err := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)
	if err != nil {
		b.Fatal(err)
	}

	defer func() {
		err = w.Stop()
		if err != nil {
			b.Errorf("error stopping the Process: error %v", err)
		}
	}()

	for b.Loop() {
		if _, err := w.Exec(context.Background(), &payload.Payload{Body: []byte("hello")}); err != nil {
			b.Fail()
		}
	}
}

func Test_Echo2(t *testing.T) {
	cmd := exec.Command("php", "../../tests/client.php", "echo", "pipes")

	w, err := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		assert.NoError(t, w.Wait())
	}()
	defer func() {
		err = w.Stop()
		if err != nil {
			t.Errorf("error stopping the Process: error %v", err)
		}
	}()

	res, err := w.Exec(context.Background(), &payload.Payload{Body: []byte("hello")})

	assert.Nil(t, err)
	assert.NotNil(t, res)
	assert.NotNil(t, res.Body)
	assert.Empty(t, res.Context)

	assert.Equal(t, "hello", res.String())
}

func Test_BadPayload2(t *testing.T) {
	cmd := exec.Command("php", "../../tests/client.php", "echo", "pipes")

	w, _ := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)

	go func() {
		assert.NoError(t, w.Wait())
	}()
	defer func() {
		err := w.Stop()
		if err != nil {
			t.Errorf("error stopping the Process: error %v", err)
		}
	}()

	res, err := w.Exec(context.Background(), &payload.Payload{})
	assert.NoError(t, err)
	assert.NotNil(t, res)
}

func Test_String2(t *testing.T) {
	cmd := exec.Command("php", "../../tests/client.php", "echo", "pipes")

	w, _ := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)
	go func() {
		assert.NoError(t, w.Wait())
	}()
	defer func() {
		err := w.Stop()
		if err != nil {
			t.Errorf("error stopping the Process: error %v", err)
		}
	}()

	assert.Contains(t, w.String(), "php ../../tests/client.php echo pipes")
	assert.Contains(t, w.String(), "ready")
	assert.Contains(t, w.String(), "num_execs: 0")
}

func Test_Echo_Slow2(t *testing.T) {
	cmd := exec.Command("php", "../../tests/slow-client.php", "echo", "pipes", "10", "10")

	w, _ := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)
	go func() {
		assert.NoError(t, w.Wait())
	}()
	defer func() {
		err := w.Stop()
		if err != nil {
			t.Errorf("error stopping the Process: error %v", err)
		}
	}()

	res, err := w.Exec(context.Background(), &payload.Payload{Body: []byte("hello")})

	assert.Nil(t, err)
	assert.NotNil(t, res)
	assert.NotNil(t, res.Body)
	assert.Empty(t, res.Context)

	assert.Equal(t, "hello", res.String())
}

func Test_Broken2(t *testing.T) {
	cmd := exec.Command("php", "../../tests/client.php", "broken", "pipes")

	w, err := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}

	res, err := w.Exec(context.Background(), &payload.Payload{Body: []byte("hello")})
	assert.NotNil(t, err)
	assert.Nil(t, res)

	time.Sleep(time.Second * 3)
	assert.Error(t, w.Stop())
}

func Test_Error2(t *testing.T) {
	cmd := exec.Command("php", "../../tests/client.php", "error", "pipes")

	w, _ := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)
	go func() {
		assert.NoError(t, w.Wait())
	}()

	defer func() {
		err := w.Stop()
		if err != nil {
			t.Errorf("error stopping the Process: error %v", err)
		}
	}()

	res, err := w.Exec(context.Background(), &payload.Payload{Body: []byte("hello")})
	assert.NotNil(t, err)
	assert.Nil(t, res)

	if errors.Is(errors.SoftJob, err) == false {
		t.Fatal("error should be of type errors.ErrSoftJob")
	}
	assert.Contains(t, err.Error(), "hello")
}

func Test_NumExecs2(t *testing.T) {
	cmd := exec.Command("php", "../../tests/client.php", "echo", "pipes")

	w, _ := NewPipeFactory(log).SpawnWorkerWithContext(context.Background(), cmd)
	go func() {
		assert.NoError(t, w.Wait())
	}()
	defer func() {
		err := w.Stop()
		if err != nil {
			t.Errorf("error stopping the Process: error %v", err)
		}
	}()

	_, err := w.Exec(context.Background(), &payload.Payload{Body: []byte("hello")})
	if err != nil {
		t.Errorf("fail to execute payload: error %v", err)
	}
	assert.Equal(t, uint64(1), w.State().NumExecs())
	w.State().Transition(fsm.StateReady)

	_, err = w.Exec(context.Background(), &payload.Payload{Body: []byte("hello")})
	if err != nil {
		t.Errorf("fail to execute payload: error %v", err)
	}
	assert.Equal(t, uint64(2), w.State().NumExecs())
	w.State().Transition(fsm.StateReady)

	_, err = w.Exec(context.Background(), &payload.Payload{Body: []byte("hello")})
	if err != nil {
		t.Errorf("fail to execute payload: error %v", err)
	}
	assert.Equal(t, uint64(3), w.State().NumExecs())
	w.State().Transition(fsm.StateReady)
}
