package static_pool

import (
	"log/slog"
	"os/exec"
	"testing"

	"github.com/roadrunner-server/pool/v2/ipc/pipe"
	"github.com/roadrunner-server/pool/v2/payload"
	"github.com/stretchr/testify/assert"
)

func FuzzStaticPoolEcho(f *testing.F) {
	f.Add([]byte("hello"))

	p, err := NewPool(
		f.Context(),
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/client.php", "echo", "pipes") },
		pipe.NewPipeFactory(slog.Default()),
		testCfg,
		slog.Default(),
	)
	assert.NoError(f, err)
	assert.NotNil(f, p)

	sc := make(chan struct{})
	f.Fuzz(func(t *testing.T, data []byte) {
		// data can't be empty
		if len(data) == 0 {
			data = []byte("1")
		}

		respCh, err := p.Exec(t.Context(), &payload.Payload{Body: data}, sc)
		assert.NoError(t, err)
		res := <-respCh
		assert.NotNil(t, res)
		assert.NotNil(t, res.Body())
		assert.Empty(t, res.Context())

		assert.Equal(t, data, res.Body())
	})
	f.Cleanup(func() { p.Destroy(f.Context()) })
}
