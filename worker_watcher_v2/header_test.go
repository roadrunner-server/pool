package workerwatcherv2

import (
	"os/exec"
	"testing"

	"github.com/roadrunner-server/pool/worker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHeader_GetWorkerChanVec(t *testing.T) {
	h := NewHeader(64)

	for range 2 {
		cmd := exec.Command("php", "../tests/client.php", "broken", "pipes")
		w, err := worker.InitBaseWorker(cmd)
		assert.NoError(t, err)
		require.NoError(t, h.PushWorker(w))
	}

	for range 10 {
		_, err := h.PopWorker()
		assert.NoError(t, err)
	}

	for range 10 {
		_, err := h.PopWorker()
		assert.NoError(t, err)
	}

	_, err := h.PopWorker()
	assert.NoError(t, err)
}
