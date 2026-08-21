package pipe

import (
	"context"
	"os/exec"

	"log/slog"

	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/goridge/v4/pkg/pipe"
	"github.com/roadrunner-server/pool/v2/fsm"
	"github.com/roadrunner-server/pool/v2/internal"
	"github.com/roadrunner-server/pool/v2/worker"
)

// Factory connects to stack using standard
// streams (STDIN, STDOUT pipes).
type Factory struct {
	log *slog.Logger
}

// NewPipeFactory returns a new factory instance.
func NewPipeFactory(log *slog.Logger) *Factory {
	return &Factory{
		log: log,
	}
}

type sr struct {
	w   *worker.Process
	err error
}

// SpawnWorkerWithContext Creates a new Process and connects it to goridge relay,
// method Wait() must be handled on the level above.
func (f *Factory) SpawnWorkerWithContext(ctx context.Context, cmd *exec.Cmd, options ...worker.Options) (*worker.Process, error) {
	spCh := make(chan sr)
	go func() {
		send := func(res sr) bool {
			select {
			case spCh <- res:
				return true
			case <-ctx.Done():
				return false
			}
		}

		w, err := worker.InitBaseWorker(cmd, options...)
		if err != nil {
			send(sr{err: err})
			return
		}

		in, err := cmd.StdoutPipe()
		if err != nil {
			send(sr{err: err})
			return
		}

		out, err := cmd.StdinPipe()
		if err != nil {
			send(sr{err: err})
			return
		}

		// Init new PIPE relay
		relay := pipe.NewPipeRelay(in, out)
		w.AttachRelay(relay)

		// Start the worker
		err = w.Start()
		if err != nil {
			send(sr{err: err})
			return
		}

		// used as a ping
		stopKill := context.AfterFunc(ctx, func() {
			_ = w.Kill()
		})
		_, err = internal.Pid(relay)
		if !stopKill() && err == nil {
			// the kill callback already ran; the worker is unusable despite the completed
			// handshake
			go func() {
				_ = w.Wait()
			}()
			send(sr{err: errors.E(errors.TimeOut)})
			return
		}
		if err != nil {
			go func() {
				_ = w.Wait()
			}()
			_ = w.Kill()
			send(sr{err: err})
			return
		}

		// everything ok, set ready state
		w.State().Transition(fsm.StateReady)

		if !send(sr{w: w}) {
			// the receiver timed out; reap the fully spawned worker
			go func() {
				_ = w.Wait()
			}()
			_ = w.Kill()
		}
	}()

	select {
	case <-ctx.Done():
		return nil, errors.E(errors.TimeOut)
	case res := <-spCh:
		if res.err != nil {
			return nil, res.err
		}
		return res.w, nil
	}
}

// Close the factory.
func (f *Factory) Close() error {
	return nil
}
