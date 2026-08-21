package static_pool

import (
	"context"

	"github.com/roadrunner-server/pool/v2/fsm"
	"github.com/roadrunner-server/pool/v2/payload"
	"github.com/roadrunner-server/pool/v2/worker"
)

type PExec struct {
	pld *payload.Payload
	err error
}

func newPExec(pld *payload.Payload, err error) *PExec {
	return &PExec{
		pld: pld,
		err: err,
	}
}

func (p *PExec) Payload() *payload.Payload {
	return p.pld
}

func (p *PExec) Body() []byte {
	return p.pld.Body
}

func (p *PExec) Context() []byte {
	return p.pld.Context
}

func (p *PExec) Error() error {
	return p.err
}

func (sp *Pool) streamIterate(ctx context.Context, w *worker.Process, resp chan *PExec, stopCh chan struct{}) {
	cancelStream := func() {
		ctxT, cancelT := context.WithTimeout(context.Background(), sp.cfg.StreamTimeout)
		err := w.StreamCancel(ctxT)
		cancelT()
		if err != nil {
			w.State().Transition(fsm.StateErrored)
			sp.log.Warn("stream cancel error", "error", err)
			return
		}

		w.State().Transition(fsm.StateReady)
		sp.log.Debug("transition to the ready state", "from", w.State().String())
	}

	// trySend delivers a frame unless the request context is gone; a blocking send on an
	// abandoned channel would pin the worker and this goroutine forever
	trySend := func(pe *PExec) bool {
		select {
		case resp <- pe:
			return true
		case <-ctx.Done():
			return false
		}
	}

	for {
		select {
		case <-stopCh:
			sp.log.Debug("stream stop signal received", "pid", w.Pid(), "state", w.State().String())
			cancelStream()
			return
		default:
			var pld *payload.Payload
			var next bool
			var errI error

			if sp.supervisedExec {
				ctxT, cancelT := context.WithTimeout(context.Background(), sp.cfg.Supervisor.ExecTTL)
				pld, next, errI = w.StreamIterWithContext(ctxT)
				cancelT()
			} else {
				// non supervised execution, can potentially hang here
				pld, next, errI = w.StreamIter()
			}

			if errI != nil {
				sp.log.Warn("stream iter error", "error", errI)
				trySend(newPExec(nil, errI))
				w.State().Transition(fsm.StateInvalid)
				return
			}

			sent := trySend(newPExec(pld, nil))
			if !next {
				// the stream is complete on the worker side regardless of delivery; there
				// is nothing to cancel anymore
				if !sent {
					select {
					case resp <- newPExec(nil, ctx.Err()):
					default:
					}
				}
				w.State().Transition(fsm.StateReady)
				return
			}

			if !sent {
				sp.log.Debug("stream consumer is gone", "pid", w.Pid())
				select {
				case resp <- newPExec(nil, ctx.Err()):
				default:
				}
				cancelStream()
				return
			}
		}
	}
}
