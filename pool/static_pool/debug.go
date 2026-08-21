package static_pool

import (
	"context"

	"github.com/roadrunner-server/events"
	"github.com/roadrunner-server/goridge/v4/pkg/frame"
	"github.com/roadrunner-server/pool/v2/payload"
)

// execDebug handles debug mode: a fresh worker is spawned for the request and destroyed after
// the response is received.
func (sp *Pool) execDebug(ctx context.Context, p *payload.Payload, stopCh chan struct{}) (chan *PExec, error) {
	sp.log.Debug("executing in debug mode, worker will be destroyed after response is received")
	w, err := sp.allocator()
	if err != nil {
		return nil, err
	}

	go func() {
		// read the exit status to prevent process to become a zombie
		_ = w.Wait()
	}()

	execCtx := context.Background()
	if sp.supervisedExec {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, sp.cfg.Supervisor.ExecTTL)
		defer cancel()
	}
	rsp, err := w.Exec(execCtx, p)
	if err != nil {
		// the worker exists only for this request; reap it on failure too
		_ = w.Kill()
		return nil, err
	}
	stopWorker := func() {
		errD := w.Stop()
		if errD != nil {
			sp.log.Debug(
				"debug mode: worker stopped with error",
				"reason", "worker error",
				"pid", w.Pid(),
				"internal_event_name", events.EventWorkerError.String(),
				"error", errD,
			)
		}
	}

	switch {
	case rsp.Flags&frame.STREAM != 0:
		// buffered so a few frames are ready ahead of the consumer
		resp := make(chan *PExec, 5)
		// send the initial frame
		resp <- newPExec(rsp, nil)

		// in case of stream, the worker is destroyed when the stream finishes
		go func() {
			defer func() {
				sp.log.Debug("stopping [stream] worker", "pid", w.Pid(), "state", w.State().String())
				close(resp)
				stopWorker()
			}()

			sp.streamIterate(ctx, w, resp, stopCh)
		}()

		return resp, nil
	default:
		resp := make(chan *PExec, 1)
		resp <- newPExec(rsp, nil)
		// close the channel
		close(resp)

		stopWorker()

		return resp, nil
	}
}
