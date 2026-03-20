package static_pool

import (
	"log/slog"
	"testing"

	"github.com/roadrunner-server/pool/v2/pool"
	"github.com/stretchr/testify/assert"
)

func TestWithLogger_SetsLogger(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	p := &Pool{cfg: &pool.Config{}}
	WithLogger(logger)(p)
	assert.Equal(t, logger, p.log)
}

func TestWithQueueSize_SetsMaxQueueSize(t *testing.T) {
	p := &Pool{cfg: &pool.Config{}}
	WithQueueSize(42)(p)
	assert.Equal(t, uint64(42), p.maxQueueSize.Load())
}

func TestWithQueueSize_Zero(t *testing.T) {
	p := &Pool{cfg: &pool.Config{}}
	WithQueueSize(100)(p)
	WithQueueSize(0)(p)
	assert.Equal(t, uint64(0), p.maxQueueSize.Load())
}

func TestWithNumWorkers_SetsNumWorkers(t *testing.T) {
	cfg := &pool.Config{NumWorkers: 1}
	p := &Pool{cfg: cfg}
	WithNumWorkers(8)(p)
	assert.Equal(t, uint64(8), p.cfg.NumWorkers)
}
