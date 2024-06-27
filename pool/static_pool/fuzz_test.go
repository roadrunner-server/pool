package static_pool //nolint:stylecheck

import (
	"context"
	"os/exec"
	"testing"

	"github.com/roadrunner-server/pool/ipc/pipe"
	"github.com/roadrunner-server/pool/payload"
	"github.com/stretchr/testify/assert"
)

func FuzzStaticPoolEcho(f *testing.F) {
	f.Add([]byte("hello"))

	ctx := context.Background()
	p, err := NewPool(
		ctx,
		func(cmd []string) *exec.Cmd { return exec.Command("php", "../../tests/client.php", "echo", "pipes") },
		pipe.NewPipeFactory(log()),
		testCfg,
		log(),
	)
	assert.NoError(f, err)
	assert.NotNil(f, p)

	sc := make(chan struct{})
	f.Fuzz(func(t *testing.T, data []byte) {
		// data can't be empty
		if len(data) == 0 {
			data = []byte("1")
		}

		respCh, err := p.Exec(ctx, &payload.Payload{Body: data}, sc)
		assert.NoError(t, err)
		res := <-respCh
		assert.NotNil(t, res)
		assert.NotNil(t, res.Body())
		assert.Empty(t, res.Context())

		assert.Equal(t, data, res.Body())
	})

	p.Destroy(ctx)
}
