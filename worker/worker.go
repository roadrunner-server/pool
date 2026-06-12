package worker

import (
	stderr "errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/pool/v2/fsm"
)

// defaultStopTimeout is how long Stop() waits for the process to exit after SIGTERM before sending SIGKILL.
const defaultStopTimeout = 10 * time.Second

// Process - supervised OS process. The process carries no IPC with RoadRunner:
// its stdout/stderr are passed through to the parent process and its lifecycle
// (start, supervision, stop) is the only pool concern.
type Process struct {
	// created indicates at what time Process has been created.
	created time.Time
	log     *slog.Logger

	callback func()
	// fsm holds information about current Process state,
	// number of Process executions, buf status change time.
	// publicly this object is receive-only and protected using Mutex
	// and atomic counter.
	fsm *fsm.Fsm

	// underlying command with associated process, command must be
	// provided to Process from outside in non-started form.
	cmd *exec.Cmd

	// pid of the process, points to pid of underlying process and
	// can be nil while process is not started.
	pid int

	doneCh chan struct{}
	// stopTimeout is how long Stop() waits for the process to exit after SIGTERM before sending SIGKILL.
	stopTimeout time.Duration
}

// InitBaseWorker creates new Process over given exec.cmd.
func InitBaseWorker(cmd *exec.Cmd, options ...Options) (*Process, error) {
	if cmd.Process != nil {
		return nil, fmt.Errorf("can't attach to running process")
	}

	w := &Process{
		created:     time.Now(),
		cmd:         cmd,
		doneCh:      make(chan struct{}, 1),
		stopTimeout: defaultStopTimeout,
	}

	// add options
	for i := range options {
		options[i](w)
	}

	if w.log == nil {
		w.log = slog.Default()
	}

	w.fsm = fsm.NewFSM(fsm.StateInactive, w.log)

	// pass the worker output through to the parent process unless the embedder
	// pre-set its own writers; an *os.File is inherited by the child directly,
	// so there is no copy goroutine and no logging layer in between
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}

	return w, nil
}

// Pid returns worker pid.
func (w *Process) Pid() int64 {
	return int64(w.pid)
}

func (w *Process) AddCallback(cb func()) {
	w.callback = cb
}

func (w *Process) Callback() {
	if w.callback == nil {
		return
	}
	w.callback()
}

// Created returns time, worker was created at.
func (w *Process) Created() time.Time {
	return w.created
}

// State return receive-only Process state object, state can be used to safely access
// Process status, time when status changed and number of Process executions.
func (w *Process) State() *fsm.Fsm {
	return w.fsm
}

// String returns Process description. fmt.Stringer interface
func (w *Process) String() string {
	st := w.fsm.String()
	// we can safely compare pid to 0
	if w.pid != 0 {
		st = st + ", pid:" + strconv.Itoa(w.pid)
	}

	return fmt.Sprintf(
		"(`%s` [%s], num_execs: %v)",
		strings.Join(w.cmd.Args, " "),
		st,
		w.fsm.NumExecs(),
	)
}

// Start starts underlying process.
func (w *Process) Start() error {
	err := w.cmd.Start()
	if err != nil {
		return err
	}
	w.pid = w.cmd.Process.Pid
	return nil
}

// Wait must be called once for each Process, call will be released once Process is
// complete and will return process error (if any). Method will return error code
// if the process fails to find or Start the script.
func (w *Process) Wait() error {
	const op = errors.Op("process_wait")
	err := w.cmd.Wait()
	w.doneCh <- struct{}{}

	// If worker was destroyed, just exit
	if w.State().Compare(fsm.StateDestroyed) {
		return nil
	}

	// If state is different, and err is not nil, append it to the errors
	if err != nil {
		w.State().Transition(fsm.StateErrored)
		err = stderr.Join(err, errors.E(op, err))
	}

	if w.cmd.ProcessState.Success() {
		w.State().Transition(fsm.StateStopped)
	}

	return err
}

// Stop gracefully terminates the worker: sends SIGTERM, waits for the process exit
// (rendezvous with Wait() via doneCh) and sends SIGKILL after the stop timeout.
// On platforms without SIGTERM delivery (Windows) or if the process is already
// gone, it degrades to SIGKILL immediately and still waits for the rendezvous.
func (w *Process) Stop() error {
	const op = errors.Op("process_stop")
	w.fsm.Transition(fsm.StateStopping)

	w.log.Debug("sending SIGTERM to the worker", "pid", w.pid)
	if err := w.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		// process already exited, or platform doesn't support SIGTERM (Windows)
		_ = w.cmd.Process.Kill()
	}

	select {
	// the doneCh send is made in the Wait() method after the process exits
	case <-w.doneCh:
		w.fsm.Transition(fsm.StateStopped)
		w.log.Debug("worker stopped", "pid", w.pid)
		return nil
	case <-time.After(w.stopTimeout):
		w.log.Warn("worker did not exit after SIGTERM, killing it", "pid", w.pid)
		_ = w.cmd.Process.Kill()
		w.fsm.Transition(fsm.StateStopped)
		return errors.E(op, errors.Str("worker did not stop within the stop timeout and was killed"))
	}
}

// Kill kills underlying process. Does not wait for process completion!
func (w *Process) Kill() error {
	w.fsm.Transition(fsm.StateStopping)
	err := w.cmd.Process.Kill()
	if err != nil {
		return err
	}
	w.fsm.Transition(fsm.StateStopped)
	return nil
}
