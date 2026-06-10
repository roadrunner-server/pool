package internal

import (
	"io"
	"strings"
	"testing"

	"github.com/roadrunner-server/goridge/v4/pkg/frame"
)

// eofRelay accepts the outgoing control frame and then fails the read,
// mimicking a worker that exited on an unparseable (legacy-protocol) frame.
type eofRelay struct{}

func (eofRelay) Send(*frame.Frame) error    { return nil }
func (eofRelay) Receive(*frame.Frame) error { return io.EOF }
func (eofRelay) Close() error               { return nil }

func TestPidHandshakeHintsLegacyWorker(t *testing.T) {
	_, err := Pid(eofRelay{})
	if err == nil {
		t.Fatal("expected the pid handshake to fail")
	}
	if !strings.Contains(err.Error(), "goridge v3") {
		t.Fatalf("handshake error should hint at the legacy protocol, got: %v", err)
	}
}
