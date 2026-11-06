package host

import (
	"bufio"
	"errors"
	"strings"
	"testing"
	"time"
)

func serveHistoricalControl(e *endpoint, epoch epochRecord) (resultErr error) {
	conn, err := e.listener.AcceptUnix()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, conn.Close()) }()
	if err = conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	var request wireRequest
	if err = readFrame(bufio.NewReader(conn), &request); err != nil {
		return err
	}
	if request.BuildID != epoch.BuildID || request.Epoch != epoch.Epoch || request.RootID != epoch.RootID {
		return ErrIdentity
	}
	// Protocol fixture only: no runtime or readiness authority exists here.
	status := Status{State: "degraded", Protocol: epoch.Protocol, Epoch: epoch.Epoch, Host: epoch.Host, BuildID: epoch.BuildID}
	return writeFrame(conn, wireResponse{Status: statusWire(status)})
}

func TestHostLegacyControlEchoDoesNotNeedCurrentExecutableDigest(t *testing.T) {
	config := testConfig(t, "hold")
	endpoint, err := newEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := endpoint.close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	epoch := epochFixture(t, config, endpoint)
	epoch.BuildID = strings.Repeat("a", 64) // Immutable historical record data.
	journal := journalForStore(t, config.Store)
	if _, err = publishRecord(t.Context(), journal, epochName(1), epoch); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var servedErr error
	t.Cleanup(func() {
		if closeErr := endpoint.close(); closeErr != nil {
			t.Error(closeErr)
		}
		receive(t, done)
		if servedErr != nil {
			t.Error(servedErr)
		}
	})
	go func() { defer close(done); servedErr = serveHistoricalControl(endpoint, epoch) }()
	status, err := Observe(t.Context(), queryFor(t, config))
	if err != nil || status.BuildID != epoch.BuildID || status.Epoch != epoch.Epoch || status.State != "degraded" || status.LastError != "" {
		t.Fatalf("historical control required current executable contents: %+v %v", status, err)
	}
	if err = CheckCompatible(status); err != nil {
		t.Fatalf("compatible historical host was refused: %v", err)
	}
}
