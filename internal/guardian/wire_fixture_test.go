package guardian

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

func cleanupLifetimeTestResource(t *testing.T, resource io.Closer) {
	t.Helper()
	t.Cleanup(func() {
		if err := resource.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Errorf("close test resource: %v", err)
		}
	})
}

// The test owns one client Close even when it also closes explicitly to assert
// scope disposal before the rest of the scenario.
func ownLifetimeWireClient(t *testing.T, client *WorkerClient) func() error {
	t.Helper()
	var once sync.Once
	var result error
	closeClient := func() error { once.Do(func() { result = client.Close() }); return result }
	t.Cleanup(func() {
		if err := closeClient(); err != nil {
			t.Error(err)
		}
	})
	return closeClient
}

// Release the callback and wake protocol I/O before joining its test actor.
func cleanupLifetimeWireWork(t *testing.T, release func(), connection net.Conn, done <-chan struct{}, label string) {
	t.Helper()
	t.Cleanup(func() {
		release()
		if err := connection.Close(); err != nil {
			t.Error(err)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("%s did not join", label)
		}
	})
}
