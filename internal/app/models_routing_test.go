package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestModelDiscoveryPrivatePreparationUsesCapturedRoutingFault(t *testing.T) {
	t.Setenv(infrastructure.OriginEnvironment, "malformed original invocation")
	clientPath := filepath.Join(supervisorfixture.CanonicalTempDir(t), "pueue")
	writeRunnerFixture(t, clientPath, "#!/bin/sh\nprintf 'pueue 4.0.4\\n'\n")
	writeRunnerFixture(t, filepath.Join(filepath.Dir(clientPath), "pueued"), "#!/bin/sh\nexit 0\n")
	deps := (Dependencies{Environment: []string{}, InitialSupervisorExecutable: clientPath}).withCapturedEnvironmentContext(t.Context())
	if deps.invocationSource == nil || !errors.Is(deps.invocationSource.Err(), infrastructure.ErrIdentity) {
		t.Fatal("fixture did not capture malformed actual source")
	}
	store := supervisorfixture.NewInspectionStore(t)
	client, err := preparePrivateAdmissionSupervisor(deps, store, pueue.Options{Environment: []string{}})
	if client != nil {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(err, infrastructure.ErrIdentity) {
		t.Fatalf("model discovery bypassed independent routing: %v", err)
	}
	if _, statErr := os.Lstat(pueue.PrivateConfigPath(store.Root)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid source prepared private daemon configuration: %v", statErr)
	}
}

func TestModelDiscoveryCleanupRetainsRoutingStore(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	other, err := taskdir.OpenStore(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := errors.Join(store.Close(), other.Close()); closeErr != nil {
			t.Error(closeErr)
		}
	})
	cleanup := &routingCleanupTestError{store: store, cause: os.ErrPermission}
	cause := errors.Join(errors.New("earlier independent cause"), cleanup)
	if err = closeModelDiscoveryStore(cause, store); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("model discovery lost cleanup cause: %v", err)
	}
	if _, pinErr := store.PinRootContext(t.Context()); pinErr != nil {
		t.Fatalf("model discovery closed routed cleanup Store: %v", pinErr)
	}
	if err = closeModelDiscoveryStore(cause, other); !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	if _, pinErr := other.PinRootContext(t.Context()); !errors.Is(pinErr, os.ErrClosed) {
		t.Fatalf("model discovery retained a different Store handle: %v", pinErr)
	}
	// This is a settled test view; the real routing error retains failed owners.
	cleanup.store = nil
	if err = closeModelDiscoveryStore(cause, store); !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	if _, pinErr := store.PinRootContext(t.Context()); !errors.Is(pinErr, os.ErrClosed) {
		t.Fatalf("model discovery retained settled view: %v", pinErr)
	}
}
