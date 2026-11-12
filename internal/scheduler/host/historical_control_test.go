package host

import (
	"bufio"
	"errors"
	"strings"
	"testing"
	"time"
)

func serveHistoricalControl(e *endpoint, epoch epochRecord) error {
	return serveHistoricalStatus(e, epoch, "degraded", "")
}

func serveHistoricalStatus(e *endpoint, epoch epochRecord, state, code string) (resultErr error) {
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
	status := Status{State: state, HostReady: state == "ready", StopAccepted: state == "stopping", Protocol: epoch.Protocol, Epoch: epoch.Epoch, Host: epoch.Host, BuildID: epoch.BuildID}
	return writeFrame(conn, wireResponse{Status: statusWire(status), Code: code})
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

func TestObserveMissingHealthRetainsLifecycleWithoutClaimingReady(t *testing.T) {
	for _, tc := range []struct {
		state, code, health string
		wantError           bool
	}{
		{state: "ready", health: "unknown"},
		{state: "stopping", health: "unknown"},
		{state: "ready", code: "invalid_control", health: "unsupported"},
		{state: "ready", code: "identity", wantError: true},
	} {
		t.Run(tc.state+tc.code, func(t *testing.T) {
			status, err := observeHistoricalStatus(t, tc.state, tc.code)
			if tc.wantError {
				if !errors.Is(err, ErrIdentity) || status.Health != nil {
					t.Fatalf("identity failure became unsupported health: %+v %v", status, err)
				}
				return
			}
			wantState := tc.state
			if tc.state == "ready" {
				wantState = "degraded"
			}
			if err != nil || status.State != wantState || status.Ready || status.Fault || status.HostReady != (tc.state == "ready") || status.StopAccepted != (tc.state == "stopping") || status.Health == nil || status.Health.State != tc.health {
				t.Fatalf("missing health claimed readiness or lost lifecycle: %+v err=%v", status, err)
			}
		})
	}
}

func observeHistoricalStatus(t *testing.T, state, code string) (Status, error) {
	t.Helper()
	config := testConfig(t, "hold")
	endpoint, err := newEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	epoch := epochFixture(t, config, endpoint)
	journal := journalForStore(t, config.Store)
	if _, err = publishRecord(t.Context(), journal, epochName(1), epoch); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	t.Cleanup(func() {
		if closeErr := endpoint.close(); closeErr != nil {
			t.Error(closeErr)
		}
		if serveErr := receive(t, done); serveErr != nil {
			t.Error(serveErr)
		}
	})
	go func() { done <- serveHistoricalStatus(endpoint, epoch, state, code) }()
	return Observe(t.Context(), queryFor(t, config))
}
