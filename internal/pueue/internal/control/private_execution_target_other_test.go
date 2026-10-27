//go:build !linux

package control

import (
	"context"
	"errors"
	"os"
	"testing"
)

// Without a natively verified peer-credential primitive, a live recorded
// daemon stays unknown instead of trusting the PID record alone.
func TestObservePrivateExecutionTargetUnsupportedPeerIsUnknown(t *testing.T) {
	fixture := newExecutionTargetFixture(t)
	fixture.recordPID(t, os.Getpid())
	fixture.listen(t)
	target, err := ObservePrivateExecutionTarget(context.Background(), fixture.stateRoot, Options{})
	assertExecutionTargetUnknown(t, target, err, nil, "")
	if !errors.Is(err, errPrivateEndpointPeerUnsupported) {
		t.Fatalf("observation error = %v, want unsupported peer identity", err)
	}
}
