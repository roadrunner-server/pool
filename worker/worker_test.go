package worker

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/roadrunner-server/pool/v2/fsm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_OnStarted(t *testing.T) {
	cmd := exec.Command("sleep", "300")
	require.NoError(t, cmd.Start())
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	_, err := InitBaseWorker(cmd)
	require.Error(t, err)
	assert.Equal(t, "can't attach to running process", err.Error())
}

func Test_String(t *testing.T) {
	w, err := InitBaseWorker(exec.Command("sleep", "300"))
	require.NoError(t, err)

	assert.Contains(t, w.String(), "sleep 300")
	assert.Contains(t, w.String(), "inactive")
	assert.Contains(t, w.String(), "num_execs: 0")
}

func Test_Stdio_DefaultInherit(t *testing.T) {
	cmd := exec.Command("sleep", "300")
	_, err := InitBaseWorker(cmd)
	require.NoError(t, err)

	// child stdio is passed through to the parent process by default
	assert.Equal(t, os.Stdout, cmd.Stdout)
	assert.Equal(t, os.Stderr, cmd.Stderr)
}

func Test_Stdio_RespectsPreset(t *testing.T) {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)

	cmd := exec.Command("sh", "-c", "echo out; echo err 1>&2")
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	w, err := InitBaseWorker(cmd)
	require.NoError(t, err)
	require.NoError(t, w.Start())
	_ = w.Wait()

	assert.Equal(t, "out\n", stdout.String())
	assert.Equal(t, "err\n", stderr.String())
	assert.True(t, w.State().Compare(fsm.StateStopped))
}

func Test_Stop_Graceful(t *testing.T) {
	// the sleep must be backgrounded: shell traps don't fire until the foreground
	// command exits. The trap also kills the sleep so no orphan outlives the shell.
	w, err := InitBaseWorker(exec.Command("sh", "-c", `trap 'kill $!; exit 0' TERM; sleep 300 & wait $!`))
	require.NoError(t, err)
	require.NoError(t, w.Start())

	go func() {
		_ = w.Wait()
	}()

	// let the shell install the trap
	time.Sleep(time.Millisecond * 300)

	start := time.Now()
	err = w.Stop()
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.True(t, w.State().Compare(fsm.StateStopped))
}

func Test_Stop_KillFallback(t *testing.T) {
	// the ignored-TERM disposition survives exec, so the single resulting sleep
	// process ignores SIGTERM -> only the SIGKILL fallback stops it
	w, err := InitBaseWorker(
		exec.Command("sh", "-c", `trap '' TERM; exec sleep 300`),
		WithStopTimeout(time.Millisecond*200),
	)
	require.NoError(t, err)
	require.NoError(t, w.Start())

	waitErr := make(chan error, 1)
	go func() {
		waitErr <- w.Wait()
	}()

	// let the shell install the trap
	time.Sleep(time.Millisecond * 300)

	err = w.Stop()
	require.Error(t, err)
	assert.True(t, w.State().Compare(fsm.StateStopped))

	select {
	case werr := <-waitErr:
		// the process was SIGKILLed
		require.Error(t, werr)
	case <-time.After(5 * time.Second):
		t.Fatal("worker was not killed after the stop timeout")
	}
}

func Test_Stop_AlreadyDead(t *testing.T) {
	w, err := InitBaseWorker(exec.Command("sh", "-c", "true"))
	require.NoError(t, err)
	require.NoError(t, w.Start())
	require.NoError(t, w.Wait())

	// the doneCh token is already buffered, Stop returns immediately
	start := time.Now()
	require.NoError(t, w.Stop())
	assert.Less(t, time.Since(start), time.Second)
}

func Test_Wait_States(t *testing.T) {
	w, err := InitBaseWorker(exec.Command("sh", "-c", "exit 0"))
	require.NoError(t, err)
	require.NoError(t, w.Start())
	require.NoError(t, w.Wait())
	assert.True(t, w.State().Compare(fsm.StateStopped))

	w, err = InitBaseWorker(exec.Command("sh", "-c", "exit 1"))
	require.NoError(t, err)
	require.NoError(t, w.Start())
	require.Error(t, w.Wait())
	assert.True(t, w.State().Compare(fsm.StateErrored))
}

func Test_Kill(t *testing.T) {
	w, err := InitBaseWorker(exec.Command("sleep", "300"))
	require.NoError(t, err)
	require.NoError(t, w.Start())

	waitErr := make(chan error, 1)
	go func() {
		waitErr <- w.Wait()
	}()

	require.NoError(t, w.Kill())

	select {
	case werr := <-waitErr:
		require.Error(t, werr)
	case <-time.After(5 * time.Second):
		t.Fatal("worker was not killed")
	}
}
