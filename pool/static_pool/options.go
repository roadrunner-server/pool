package static_pool

import (
	"log/slog"
)

type Options func(p *Pool)

func WithLogger(logger *slog.Logger) Options {
	return func(p *Pool) {
		p.log = logger
	}
}

func WithQueueSize(l uint64) Options {
	return func(p *Pool) {
		p.maxQueueSize.Store(l)
	}
}

func WithNumWorkers(l uint64) Options {
	return func(p *Pool) {
		p.cfg.NumWorkers = l
	}
}
