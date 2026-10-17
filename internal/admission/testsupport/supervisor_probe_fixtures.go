package testsupport

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func ListenSupervisorProbeTestEndpoint(t *testing.T, endpoint string) {
	t.Helper()
	const prefix = "unix:"
	if !strings.HasPrefix(endpoint, prefix) {
		t.Fatalf("private supervisor endpoint = %q, want Unix socket", endpoint)
	}
	listener, err := net.Listen("unix", strings.TrimPrefix(endpoint, prefix))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			if closeErr := connection.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
	}()
	t.Cleanup(func() {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Error(closeErr)
		}
		<-done
	})
}

func SettleSupervisorProbeTestClient(t *testing.T, client *pueue.Client, err error) {
	t.Helper()
	var inFlight *pueue.InFlightError
	if errors.As(err, &inFlight) && inFlight.Pending != nil {
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		select {
		case <-inFlight.Pending.Done():
		case <-timer.C:
			t.Fatalf("finite supervisor fixture did not complete: %v", err)
		}
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
}
