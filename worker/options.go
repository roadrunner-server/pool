package worker

import (
	"log/slog"
	"math/rand/v2"
)

const (
	maxExecsPercentJitter uint64 = 15
)

type Options func(p *Process)

func WithLog(z *slog.Logger) Options {
	return func(p *Process) {
		p.log = z
	}
}

func WithMaxExecs(maxExecs uint64) Options {
	return func(p *Process) {
		percent := rand.Uint64N(maxExecsPercentJitter) //nolint:gosec // This value only sets the worker restart limit.
		p.maxExecs = maxExecs + uint64(float64(maxExecs)*float64(percent)/100)
	}
}
