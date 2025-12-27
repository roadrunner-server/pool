package workerwatcherv2

import (
	"errors"
	"math/bits"
	"sync/atomic"

	"github.com/roadrunner-server/pool/worker"
)

type Header struct {
	workers []*worker.Process
	bitmask uint64
}

func NewHeader(allocWorkers uint) *Header {
	if allocWorkers > 64 {
		panic("too many workers")
	}

	return &Header{
		workers: make([]*worker.Process, allocWorkers),
	}
}

func (h *Header) PopWorker() (*worker.Process, error) {
	slot := bits.TrailingZeros64(h.bitmask)
	if slot >= len(h.workers) {
		return nil, errors.New("no workers")
	}
	_ = atomic.CompareAndSwapUint64(&h.bitmask, h.bitmask, h.bitmask&(^(1 << slot)))
	return h.workers[slot], nil
}

func (h *Header) PushWorker(w *worker.Process) error {
	slot := bits.TrailingZeros64(^h.bitmask)
	if len(h.workers) <= slot {
		return errors.New("too many workers")
	}

	_ = atomic.CompareAndSwapUint64(&h.bitmask, h.bitmask, h.bitmask|(1<<slot))
	h.workers[slot] = w
	return nil
}
