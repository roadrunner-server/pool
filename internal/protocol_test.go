package internal

import (
	"net"
	"testing"

	"github.com/roadrunner-server/goridge/v4/pkg/frame"
	"github.com/roadrunner-server/goridge/v4/pkg/socket"
	"github.com/stretchr/testify/require"
)

func TestPidJSON(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantPID int64
		wantErr bool
	}{
		{name: "valid pid", body: `{"pid":123}`, wantPID: 123},
		{name: "duplicate pid", body: `{"pid":123,"pid":456}`, wantErr: true},
		{name: "invalid pid", body: `{"pid":0}`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, server := net.Pipe()
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			result := make(chan error, 1)
			go func() {
				rl := socket.NewSocketRelay(server)
				fr := frame.NewFrame()
				if err := rl.Receive(fr); err != nil {
					result <- err
					return
				}
				fr.Reset()
				fr.WriteVersion(fr.Header(), frame.Version1)
				fr.WriteFlags(fr.Header(), frame.CONTROL, frame.CodecJSON)
				fr.WritePayloadLen(fr.Header(), uint32(len(tt.body))) //nolint:gosec
				fr.WritePayload([]byte(tt.body))
				fr.WriteCRC(fr.Header())
				result <- rl.Send(fr)
			}()

			pid, err := Pid(socket.NewSocketRelay(client))
			require.NoError(t, <-result)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantPID, pid)
		})
	}
}
