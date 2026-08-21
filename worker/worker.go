package worker

import (
	"bytes"
	"cmp"
	"context"
	stderr "errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/goridge/v4/pkg/frame"
	"github.com/roadrunner-server/goridge/v4/pkg/relay"
	"github.com/roadrunner-server/pool/v2/fsm"
	"github.com/roadrunner-server/pool/v2/internal"
	"github.com/roadrunner-server/pool/v2/payload"
)

// Process - supervised process with api over goridge.Relay.
type Process struct {
	// created indicates at what time Process has been created.
	created time.Time
	log     *slog.Logger

	// calculated maximum value with jitter
	maxExecs uint64

	callback func()
	// fsm holds information about current Process state,
	// number of Process executions, buf status change time.
	// publicly this object is receive-only and protected using Mutex
	// and atomic counter.
	fsm *fsm.Fsm

	// underlying command with associated process, command must be
	// provided to Process from outside in non-started form. CmdSource
	// stdErr direction will be handled by Process to aggregate error message.
	cmd *exec.Cmd

	// pid of the process, points to pid of underlying process and
	// can be nil while process is not started.
	pid int

	fPool  sync.Pool
	bPool  sync.Pool
	chPool sync.Pool

	doneCh chan struct{}

	// communication bus with underlying process.
	relay relay.Relay
}

// internal struct to pass data between goroutines
type wexec struct {
	payload *payload.Payload
	err     error
}

// InitBaseWorker creates new Process over given exec.cmd.
func InitBaseWorker(cmd *exec.Cmd, options ...Options) (*Process, error) {
	if cmd.Process != nil {
		return nil, errors.Str("can't attach to running process")
	}

	w := &Process{
		created: time.Now(),
		cmd:     cmd,
		doneCh:  make(chan struct{}, 1),

		fPool: sync.Pool{
			New: func() any {
				return frame.NewFrame()
			},
		},
		bPool: sync.Pool{
			New: func() any {
				return new(bytes.Buffer)
			},
		},
		chPool: sync.Pool{
			New: func() any {
				return make(chan *wexec, 1)
			},
		},
	}

	// add options
	for i := range options {
		options[i](w)
	}

	w.log = cmp.Or(w.log, slog.Default())

	w.fsm = fsm.NewFSM(fsm.StateInactive, w.log)

	// set self as stderr implementation (Writer interface)
	rc, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	go func() {
		// https://linux.die.net/man/7/pipe
		// see pipe capacity
		buf := make([]byte, 65536)
		errCopy := copyBuffer(w, rc, buf)
		if errCopy != nil {
			w.log.Debug("stderr", "error", errCopy)
		}
	}()

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

// AttachRelay attaches relay to the worker
func (w *Process) AttachRelay(rl relay.Relay) {
	w.relay = rl
}

// Relay returns relay attached to the worker
func (w *Process) Relay() relay.Relay {
	return w.relay
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

func (w *Process) Start() error {
	err := w.cmd.Start()
	if err != nil {
		return err
	}
	w.pid = w.cmd.Process.Pid
	return nil
}

// Wait must be called once for each Process, call will be released once Process is
// complete and will return process error (if any), if stderr is presented it is value
// will be wrapped as WorkerError. Method will return error code if php process fails
// to find or Start the script.
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

	// closeRelay
	// at this point according to the documentation (see cmd.Wait comment)
	// if worker finishes with an error, message will be written to the stderr first
	// and then process.cmd.Wait return an error
	err2 := w.closeRelay()
	if err2 != nil {
		w.State().Transition(fsm.StateErrored)
		return stderr.Join(err, errors.E(op, err2))
	}

	if w.cmd.ProcessState.Success() {
		w.State().Transition(fsm.StateStopped)
	}

	return err
}

func (w *Process) StreamIter() (*payload.Payload, bool, error) {
	pld, err := w.receiveFrame()
	if err != nil {
		return nil, false, err
	}

	// PING, we should respond with PONG
	if pld.Flags&frame.PING != 0 {
		if err := w.sendPONG(); err != nil {
			return nil, false, err
		}
	}

	// !=0 -> we have stream bit set, so stream is available
	return pld, pld.Flags&frame.STREAM != 0, nil
}

// StreamIter returns true if stream is available and payload
func (w *Process) StreamIterWithContext(ctx context.Context) (*payload.Payload, bool, error) {
	c := w.getCh()

	go func() {
		rsp, err := w.receiveFrame()
		if err != nil {
			c <- &wexec{
				err: err,
			}

			w.log.Debug("stream iter error", "pid", w.Pid(), "error", err)
			return
		}

		c <- &wexec{
			payload: rsp,
		}
	}()

	select {
	// exec TTL reached
	case <-ctx.Done():
		return nil, false, errors.E(errors.ExecTTL, w.abortExec(ctx, c))
	case res := <-c:
		w.putCh(c)

		if res.err != nil {
			return nil, false, res.err
		}

		// PING, we should respond with PONG
		if res.payload.Flags&frame.PING != 0 {
			if err := w.sendPONG(); err != nil {
				return nil, false, err
			}
		}

		// pld.Flags&frame.STREAM !=0 -> we have stream bit set, so stream is available
		return res.payload, res.payload.Flags&frame.STREAM != 0, nil
	}
}

// StreamCancel sends stop bit to the worker
func (w *Process) StreamCancel(ctx context.Context) error {
	const op = errors.Op("sync_worker_stream_cancel")
	if !w.State().Compare(fsm.StateWorking) {
		return errors.Errorf("worker is not in the Working state, actual state: (%s)", w.State().String())
	}

	w.log.Debug("stream was canceled, sending stop bit", "pid", w.Pid())
	// get a frame
	fr := w.getFrame()

	fr.WriteVersion(fr.Header(), frame.Version1)

	fr.SetStopBit(fr.Header())
	fr.WriteCRC(fr.Header())

	err := w.Relay().Send(fr)
	w.State().RegisterExec()
	if err != nil {
		w.putFrame(fr)
		return errors.E(op, errors.Network, err)
	}

	w.putFrame(fr)
	c := w.getCh()

	w.log.Debug("stop bit was sent, waiting for the response", "pid", w.Pid())

	go func() {
		for {
			rsp, errrf := w.receiveFrame()
			if errrf != nil {
				c <- &wexec{
					err: errrf,
				}
				w.log.Debug("stream cancel error", "pid", w.Pid(), "error", errrf)
				return
			}

			// stream has ended
			if rsp.Flags&frame.STREAM == 0 {
				w.log.Debug("stream has ended", "pid", w.Pid())
				c <- &wexec{}
				return
			}
		}
	}()

	select {
	// exec TTL reached
	case <-ctx.Done():
		return errors.E(op, errors.ExecTTL, w.abortExec(ctx, c))
	case res := <-c:
		w.putCh(c)
		if res.err != nil {
			return res.err
		}
		return nil
	}
}

func (w *Process) abortExec(ctx context.Context, c chan *wexec) error {
	errK := w.Kill()
	err := stderr.Join(errK, ctx.Err())
	// the channel returns an error or nil
	<-c
	w.putCh(c)
	return err
}

// Exec executes payload with TTL timeout in the context.
func (w *Process) Exec(ctx context.Context, p *payload.Payload) (*payload.Payload, error) {
	const op = errors.Op("worker_exec_with_timeout")

	// worker was killed before it started to work (supervisor)
	if !w.State().Compare(fsm.StateReady) {
		return nil, errors.E(op, errors.Retry, errors.Errorf("Process is not ready (%s)", w.State().String()))
	}

	// set last used time
	w.State().SetLastUsed(uint64(time.Now().UnixNano()))
	w.State().Transition(fsm.StateWorking)

	do := func() *wexec {
		err := w.sendFrame(p)
		if err != nil {
			return &wexec{err: err}
		}

		w.State().RegisterExec()
		rsp, err := w.receiveFrame()

		return &wexec{payload: rsp, err: err}
	}

	// fast path: a context that can never fire needs no goroutine and no channel round-trip
	if ctx.Done() == nil {
		res := do()
		if res.err != nil {
			return nil, res.err
		}

		return res.payload, nil
	}

	c := w.getCh()
	go func() {
		c <- do()
	}()

	select {
	// exec TTL reached
	case <-ctx.Done():
		return nil, errors.E(op, errors.ExecTTL, w.abortExec(ctx, c))
	case res := <-c:
		w.putCh(c)
		if res.err != nil {
			return nil, res.err
		}
		return res.payload, nil
	}
}

// Stop sends soft termination command to the Process and waits for process completion.
func (w *Process) Stop() error {
	const op = errors.Op("process_stop")
	w.fsm.Transition(fsm.StateStopping)

	go func() {
		w.log.Debug("sending stop request to the worker", "pid", w.pid)
		err := internal.SendControl(w.relay, &internal.StopCommand{Stop: true})
		if err == nil {
			w.fsm.Transition(fsm.StateStopped)
		}
	}()

	select {
	// finished, sent to the doneCh is made in the Wait() method
	// If we successfully sent a stop request, Wait() method will send a struct{} to the doneCh and we're done here
	// otherwise we have 10 seconds before we kill the process
	case <-w.doneCh:
		w.log.Debug("worker stopped", "pid", w.pid)
		return nil
	case <-time.After(time.Second * 10):
		// kill process
		w.log.Warn("worker doesn't respond on stop command, killing process", "pid", w.Pid())
		_ = w.cmd.Process.Signal(os.Kill)
		w.fsm.Transition(fsm.StateStopped)
		return errors.E(op, errors.Network)
	}
}

// Kill kills underlying process, make sure to call Wait() func to gather
// error log from the stderr. Does not wait for process completion!
func (w *Process) Kill() error {
	w.fsm.Transition(fsm.StateStopping)
	err := w.cmd.Process.Kill()
	if err != nil {
		return err
	}
	w.fsm.Transition(fsm.StateStopped)
	return nil
}

// Worker stderr
func (w *Process) Write(p []byte) (int, error) {
	// unsafe to use utils.AsString
	w.log.Info(string(p))
	return len(p), nil
}

func (w *Process) MaxExecsReached() bool {
	return w.maxExecs > 0 && w.State().NumExecs() >= w.maxExecs
}

func (w *Process) MaxExecs() uint64 {
	return w.maxExecs
}

// copyBuffer copies src to dst through buf until EOF.
func copyBuffer(dst io.Writer, src io.Reader, buf []byte) error {
	_, err := io.CopyBuffer(dst, src, buf)
	return err
}

// sendFrame sends frame to the worker
func (w *Process) sendFrame(p *payload.Payload) error {
	const op = errors.Op("sync_worker_send_frame")
	// get a frame
	fr := w.getFrame()
	buf := w.get()

	// can be 0 here
	fr.WriteVersion(fr.Header(), frame.Version1)
	fr.WriteFlags(fr.Header(), p.Codec)

	// obtain a buffer
	buf.Write(p.Context)
	buf.Write(p.Body)

	// Context offset
	fr.WriteOptions(fr.HeaderPtr(), uint32(len(p.Context))) //nolint:gosec
	fr.WritePayloadLen(fr.Header(), uint32(buf.Len()))      //nolint:gosec
	fr.WritePayload(buf.Bytes())

	fr.WriteCRC(fr.Header())

	// return buffer
	w.put(buf)

	err := w.Relay().Send(fr)
	if err != nil {
		w.putFrame(fr)
		return errors.E(op, errors.Network, err)
	}
	w.putFrame(fr)
	return nil
}

func (w *Process) receiveFrame() (*payload.Payload, error) {
	const op = errors.Op("sync_worker_receive_frame")

	frameR := w.getFrame()

	err := w.Relay().Receive(frameR)
	if err != nil {
		w.putFrame(frameR)
		return nil, errors.E(op, errors.Network, err)
	}

	codec := frameR.ReadFlags()

	if codec&frame.ERROR != byte(0) {
		// we need to copy the payload because we will put the frame back to the pool
		cp := bytes.Clone(frameR.Payload())

		w.putFrame(frameR)
		return nil, errors.E(op, errors.SoftJob, errors.Str(string(cp)))
	}

	options := frameR.ReadOptions(frameR.Header())
	if len(options) != 1 {
		w.putFrame(frameR)
		return nil, errors.E(op, errors.Decode, errors.Str("options length should be equal 1 (body offset)"))
	}

	// bound check
	if len(frameR.Payload()) < int(options[0]) {
		// we need to copy the payload because we will put the frame back to the pool
		cp := bytes.Clone(frameR.Payload())

		w.putFrame(frameR)
		return nil, errors.E(errors.Network, errors.Errorf("bad payload %s", cp))
	}

	// stream + stop -> waste
	// stream + ping -> response
	flags := frameR.Header()[10]

	// by cloning we free frame's payload slice: no pointer from the smaller slices to the
	// initial one, which goes back to the sync.Pool
	// https://blog.golang.org/slices-intro#TOC_6.
	pld := &payload.Payload{
		Flags:   flags,
		Codec:   codec,
		Body:    bytes.Clone(frameR.Payload()[options[0]:]),
		Context: bytes.Clone(frameR.Payload()[:options[0]]),
	}

	w.putFrame(frameR)
	return pld, nil
}

func (w *Process) sendPONG() error {
	// get a frame
	fr := w.getFrame()
	fr.WriteVersion(fr.Header(), frame.Version1)

	fr.SetPongBit(fr.Header())
	fr.WriteCRC(fr.Header())

	err := w.Relay().Send(fr)
	w.State().RegisterExec()
	if err != nil {
		w.putFrame(fr)
		w.State().Transition(fsm.StateErrored)
		return errors.E(errors.Network, err)
	}

	w.putFrame(fr)
	return nil
}

func (w *Process) closeRelay() error {
	if w.relay != nil {
		err := w.relay.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *Process) get() *bytes.Buffer {
	return w.bPool.Get().(*bytes.Buffer)
}

func (w *Process) put(b *bytes.Buffer) {
	b.Reset()
	w.bPool.Put(b)
}

func (w *Process) getFrame() *frame.Frame {
	return w.fPool.Get().(*frame.Frame)
}

func (w *Process) putFrame(f *frame.Frame) {
	f.Reset()
	w.fPool.Put(f)
}

func (w *Process) getCh() chan *wexec {
	return w.chPool.Get().(chan *wexec)
}

func (w *Process) putCh(ch chan *wexec) {
	// just check if the chan is not empty
	select {
	case <-ch:
	default:
	}
	w.chPool.Put(ch)
}
