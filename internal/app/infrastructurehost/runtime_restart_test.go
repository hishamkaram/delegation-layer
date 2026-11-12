package infrastructurehost

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRuntimeRestartEstablishmentFencesBothApplicationCallbacks(t *testing.T) {
	fixture := newInfrastructureSavedBindingFixture(t, infrastructure.SavedBindingTaskMetadata, true)
	base := filepath.Join(fixture.store.Root, ".supervisor")
	if err := os.Mkdir(base, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	// Historical journal data only; no process, daemon, or restart is created.
	from, err := infrastructure.FormatRuntimeRestartReference(infrastructure.RuntimeRestartReference{RootID: fixture.store.RootID, Startup: infrastructure.StartupReference{Sequence: 1, Epoch: strings.Repeat("1", 32), IntentSHA256: strings.Repeat("a", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := task.MarshalCanonical(struct {
		SchemaVersion int `json:"schema_version"`
		Restart       struct {
			From            string             `json:"from"`
			Binding         task.SupervisorRef `json:"binding"`
			Phase           string             `json:"phase"`
			OldPID          int                `json:"old_pid"`
			OldProcessStart string             `json:"old_process_start"`
		} `json:"runtime_restart"`
	}{SchemaVersion: task.SchemaVersion, Restart: struct {
		From            string             `json:"from"`
		Binding         task.SupervisorRef `json:"binding"`
		Phase           string             `json:"phase"`
		OldPID          int                `json:"old_pid"`
		OldProcessStart string             `json:"old_process_start"`
	}{From: from, Binding: fixture.binding, Phase: "requested", OldPID: 101, OldProcessStart: "recorded-only"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "bootstrap.pending")
	writeFixtureFile(t, path, data)
	if pending, readErr := pueue.PrivateRuntimeRestartPending(t.Context(), fixture.store.Root); readErr != nil || !pending {
		t.Fatalf("invalid historical fence fixture: pending=%t err=%v", pending, readErr)
	}
	before := supervisorfixture.SnapshotStore(t, fixture.store.Root)
	host := Host{}
	if err = host.ValidateEstablishment(t.Context(), fixture.store, infrastructure.InitialOperation{}); !errors.Is(err, pueue.ErrPrivateRuntimeRestartPending) {
		t.Fatalf("resident callback ignored runtime fence: %v", err)
	}
	outside := newInfrastructureEstablishmentHost("/must-not-resolve/delegate-run", infrastructure.InitialOperation{})
	if _, err = outside.PrepareOutsideRunner(t.Context(), fixture.store); !errors.Is(err, pueue.ErrPrivateRuntimeRestartPending) {
		t.Fatalf("outside callback reached runner selection during restart: %v", err)
	}
	if !maps.Equal(before, supervisorfixture.SnapshotStore(t, fixture.store.Root)) {
		t.Fatal("refused establishment changed retained state")
	}
	writeFixtureFile(t, path, []byte("{broken"))
	before = supervisorfixture.SnapshotStore(t, fixture.store.Root)
	if err = host.ValidateEstablishment(t.Context(), fixture.store, infrastructure.InitialOperation{}); err == nil {
		t.Fatal("unreadable restart record granted establishment")
	}
	if !maps.Equal(before, supervisorfixture.SnapshotStore(t, fixture.store.Root)) {
		t.Fatal("unreadable evidence was repaired or removed")
	}
}
