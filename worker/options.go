package worker

import (
	"crypto/rand"
	"log/slog"
	"math/big"
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
		p.maxExecs = calculateMaxExecsJitter(maxExecs, maxExecsPercentJitter, p.log)
	}
}

func calculateMaxExecsJitter(maxExecs, jitter uint64, log *slog.Logger) uint64 {
	if maxExecs == 0 {
		return 0
	}

	random, err := rand.Int(rand.Reader, big.NewInt(int64(jitter))) //nolint:gosec

	if err != nil {
		log.Debug("jitter calculation error", "error", err, "jitter", jitter)
		return maxExecs
	}

	percent := random.Uint64()

	if percent == 0 {
		return maxExecs
	}

	result := (float64(maxExecs) * float64(percent)) / 100.0

	return maxExecs + uint64(result)
}
