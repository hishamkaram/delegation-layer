package control

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestBindPrivateClosesRefusedEndpointOfAbsentRecordedDaemon(t *testing.T) {
	fixture := newRefusedPrivateEndpointFixture(t)
	published := fixture.client.Binding()

	client, err := fixture.bind(t)
	if err != nil {
		t.Fatalf("refused endpoint of an absent recorded daemon was not recovered: %v", err)
	}
	fixture.assertSocketRemoved(t)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	assertPrivateDaemonIdentityMatchesBinding(t, fixture.stateRoot, client.Binding())
	if current, found, readErr := loadPrivateSupervisorBinding(fixture.stateRoot); readErr != nil || !found || current != published {
		t.Fatalf("recovery changed the published generation: %+v found=%t err=%v", current, found, readErr)
	}
}

func TestRecoverPrivateEntersExclusiveClosureForRefusedEndpoint(t *testing.T) {
	fixture := newRefusedPrivateEndpointFixture(t)
	saved := fixture.client.Binding()
	ctx, cancel := context.WithTimeout(context.Background(), privateTestObservationTimeout)
	defer cancel()

	recovered, err := RecoverPrivate(ctx, fixture.stateRoot, saved, fixture.clientPath, fixture.daemonPath, Options{ObservationTimeout: privateTestObservationTimeout})
	if err != nil {
		t.Fatalf("saved recovery did not enter exclusive refused-endpoint closure: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := recovered.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if recovered.Binding() != saved {
		t.Fatalf("recovered client changed the saved binding: %+v", recovered.Binding())
	}
	fixture.assertSocketRemoved(t)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
}

func TestPendingMigrationRefusedEndpointStartsRecordedSuccessor(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	options := privateMigrationTestOptions(fixture.stateRoot, 10*time.Second)
	legacy := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath, options)
	nextPath := filepath.Join(filepath.Dir(fixture.daemonPath), "proof-successor")
	writeExecutable(t, nextPath, privateDaemonFixture+"\n# successor\n")
	current := bindPrivateSetupForTest(t, fixture.stateRoot, fixture.clientPath, nextPath, options)
	base := filepath.Dir(current.Binding().ConfigPath)
	pending := privateMigrationRecord{SchemaVersion: task.SchemaVersion, Legacy: privateDaemonIdentityForBinding(legacy.Binding()), Successor: privateDaemonIdentityForBinding(current.Binding())}
	if err := publishPrivateMigrationRecord(base, pending.Legacy, pending.Successor); err != nil {
		t.Fatal(err)
	}
	pid, _, err := legacy.privateDaemonPID()
	if err != nil {
		t.Fatal(err)
	}
	stopped := pending
	stopped.LegacyPID, stopped.LegacyProcessStart = pid, "fixture:"+strconv.Itoa(pid)
	stopped.LegacyShutdownRequested, stopped.LegacyShutdownDispatched, stopped.LegacyStopped = true, true, true
	if err = replacePrivateMigrationRecord(base, pending, stopped); err != nil {
		t.Fatal(err)
	}
	// The predecessor stopped without unlinking its socket. The recorded PID
	// belongs to the naturally exited fixture daemon.
	removePrivateTestPath(t, filepath.Join(base, "ready"))
	socketPath := strings.TrimPrefix(current.runtimeBinding.Endpoint, "unix:")
	listenRefusedPrivateTestSocket(t, socketPath)

	ctx, cancel := context.WithTimeout(context.Background(), PrivateControlTimeout(options))
	defer cancel()
	err = WithBootstrapLock(ctx, fixture.stateRoot, func() error { return current.MigratePrivateSupervisorWithLock(ctx, fixture.stateRoot) })
	if err != nil {
		t.Fatalf("stopped migration did not resume through the refused endpoint: %v", err)
	}
	assertPrivateDaemonIdentityMatchesPair(t, fixture.stateRoot, nextPath)
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	assertPrivateTestPathMissing(t, socketPath)
	if _, found, loadErr := loadPrivateMigrationRecord(base); loadErr != nil || found {
		t.Fatalf("journal not cleared: found=%t err=%v", found, loadErr)
	}
}

func TestRefusedEndpointClosureRevalidatesUnderLock(t *testing.T) {
	fixture := newRefusedPrivateEndpointFixture(t)
	removePrivateTestPath(t, fixture.socketPath)
	listener, err := net.Listen("unix", fixture.socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Error(closeErr)
		}
	})
	// A stale refusal observation cannot remove a socket that now accepts.
	err = fixture.client.closeRefusedPrivateEndpoint(context.Background(), staleRefusedObservation(t))
	if !errors.Is(err, ErrBinding) || !errors.Is(err, ErrPrivateEndpointRefused) {
		t.Fatalf("closure accepted a changed live endpoint: %v", err)
	}
	connection, err := net.Dial("unix", fixture.socketPath)
	if err != nil {
		t.Fatalf("closure disturbed the accepting endpoint: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := connection.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
}
