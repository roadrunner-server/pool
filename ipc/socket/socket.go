package socket

import (
	"context"
	stderr "errors"
	"net"
	"os/exec"
	"sync"
	"time"

	"log/slog"

	"github.com/roadrunner-server/errors"
	"github.com/roadrunner-server/goridge/v4/pkg/relay"
	"github.com/roadrunner-server/goridge/v4/pkg/socket"
	"github.com/roadrunner-server/pool/v2/fsm"
	"github.com/roadrunner-server/pool/v2/internal"
	"github.com/roadrunner-server/pool/v2/worker"
	"github.com/shirou/gopsutil/process"
)

// Factory connects to external stack using socket server.
type Factory struct {
	// listens for incoming connections from underlying processes
	ls net.Listener
	// sockets which are waiting for process association
	relays sync.Map
	log    *slog.Logger
}

// NewSocketServer returns Factory attached to a given socket listener.
func NewSocketServer(ls net.Listener, log *slog.Logger) *Factory {
	f := &Factory{
		ls:  ls,
		log: log,
	}

	go func() {
		err := f.listen()
		// the listener is closed as part of shutdown
		if stderr.Is(err, net.ErrClosed) {
			return
		}

		log.Warn("socket server listen", "error", err)
	}()

	return f
}

// blocking operation, returns an error
func (f *Factory) listen() error {
	for {
		conn, err := f.ls.Accept()
		if err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Minute))
		rl := socket.NewSocketRelay(conn)
		pid, err := internal.Pid(rl)
		if err != nil {
			f.log.Warn("failed to read the pid from the socket connection", "error", err)
			_ = conn.Close()
			continue
		}
		// the relay is reused for all later worker traffic, which sets its own bounds
		_ = conn.SetReadDeadline(time.Time{})
		f.attachRelayToPid(pid, rl)
	}
}

type socketSpawn struct {
	w   *worker.Process
	err error
}

// SpawnWorkerWithContext Creates a Process and connects it to the appropriate relay or return an error
func (f *Factory) SpawnWorkerWithContext(ctx context.Context, cmd *exec.Cmd, options ...worker.Options) (*worker.Process, error) {
	c := make(chan socketSpawn)
	go func() {
		send := func(res socketSpawn) bool {
			select {
			case c <- res:
				return true
			case <-ctx.Done():
				return false
			}
		}

		w, err := worker.InitBaseWorker(cmd, options...)
		if err != nil {
			send(socketSpawn{err: err})
			return
		}

		err = w.Start()
		if err != nil {
			send(socketSpawn{err: err})
			return
		}

		rl, err := f.findRelayWithContext(ctx, w)
		if err != nil {
			go func() {
				_ = w.Wait()
			}()
			_ = w.Kill()
			send(socketSpawn{err: err})
			return
		}

		w.AttachRelay(rl)
		w.State().Transition(fsm.StateReady)

		if !send(socketSpawn{w: w}) {
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
	case res := <-c:
		if res.err != nil {
			return nil, res.err
		}

		return res.w, nil
	}
}

// Close socket factory and underlying socket connection.
func (f *Factory) Close() error {
	return f.ls.Close()
}

// waits for Process to connect over socket and returns associated relay or timeout
func (f *Factory) findRelayWithContext(ctx context.Context, w *worker.Process) (*socket.Relay, error) {
	ticker := time.Tick(time.Millisecond * 10)
	for {
		// fast path: check relay map immediately
		rl, ok := f.relays.LoadAndDelete(w.Pid())
		if ok {
			return rl.(*socket.Relay), nil
		}

		select {
		case <-ctx.Done():
			return nil, errors.E(errors.Op("findRelayWithContext"), errors.TimeOut)
		case <-ticker:
			// check if process still exists
			_, err := process.NewProcess(int32(w.Pid())) //nolint:gosec
			if err != nil {
				return nil, err
			}
		}
	}
}

// attachRelayToPid stores the relay associated with the specific pid
func (f *Factory) attachRelayToPid(pid int64, relay relay.Relay) {
	f.relays.Store(pid, relay)
}
