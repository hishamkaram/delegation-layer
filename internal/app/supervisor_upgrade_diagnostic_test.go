package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestSupervisorUpgradeDiagnosticUsesInstalledPairAfterOldRemoval(t *testing.T) {
	store, deps, original, owner := newOwnedSupervisorProbeTestFixture(t)
	old := original.Binding()
	currentClient, currentDaemon := writePublicContinuationPair(t, owner.base, "D")
	pin, err := store.PinRootContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(map[string]any{"schema_version": task.SchemaVersion, "drain_v1": map[string]any{"root": map[string]any{"path": pin.Root(), "id": pin.RootID(), "device": pin.Device(), "inode": pin.Inode()}, "predecessor": old, "operation_id": "upgrade-v1:" + strings.Repeat("9", 32), "mode": "reactive", "phase": "draining"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(store.Root, ".supervisor", "bootstrap.pending"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{old.ClientExecutable, old.DaemonExecutable} {
		if err = os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client, err := supervisorMaintenanceClient(ctx, store, old, currentClient, currentDaemon, deps.SupervisorOptions)
	defer func() { settleSupervisorProbeTestClient(t, client, err) }()
	if err != nil || client == nil {
		t.Fatalf("deleted predecessor pair blocked installed diagnostic transport: %v", err)
	}
	if client.Binding() != old || client.RuntimeBinding().ClientExecutable != currentClient || client.RuntimeBinding().DaemonExecutable != currentDaemon {
		t.Fatalf("diagnostic changed logical provenance or selected a mixed pair: logical=%+v runtime=%+v", client.Binding(), client.RuntimeBinding())
	}
	if _, err = client.Snapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(old.ClientExecutable); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("diagnostic recreated predecessor client")
	}
	if _, err = os.Stat(old.DaemonExecutable); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("diagnostic recreated predecessor daemon")
	}
}
