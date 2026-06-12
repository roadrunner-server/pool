package worker

import (
	"log/slog"
	"time"
)

type Options func(p *Process)

func WithLog(z *slog.Logger) Options {
	return func(p *Process) {
		p.log = z
	}
}

// WithStopTimeout overrides how long Stop() waits for the process to exit
// after SIGTERM before sending SIGKILL (default 10s).
func WithStopTimeout(d time.Duration) Options {
	return func(p *Process) {
		if d > 0 {
			p.stopTimeout = d
		}
	}
}
