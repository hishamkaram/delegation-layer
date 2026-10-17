package supervisor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestGenerationResolutionKeepsPrivateAuthority(t *testing.T) {
	store := newGenerationStore(t)
	legacy := generationBinding(store.Root, "legacy")
	active := generationBinding(store.Root, "active")
	writeGenerationGroup(t, store, legacy)
	var calls []task.SupervisorRef
	resolve := func(_ context.Context, _ string, saved task.SupervisorRef) (task.SupervisorRef, bool, error) {
		calls = append(calls, saved)
		return active, true, nil
	}
	got, err := resolvePrivateSupervisorGenerationUsing(t.Context(), store, legacy, resolve)
	if err != nil {
		t.Fatal(err)
	}
	if !got.PrivateAuthority || !got.Found || got.Active != active || !got.GroupCleanupDue || got.GroupHead.Supervisor != legacy {
		t.Fatalf("private authority or pending group edge changed: %+v", got)
	}
	if len(calls) != 2 || calls[0] != legacy || calls[1] != legacy {
		t.Fatalf("private authority and exact group predecessor were not both resolved: %+v", calls)
	}
	head, migrated, err := inspection.ReadGroupHead(t.Context(), store)
	if err != nil || migrated || head.Supervisor != legacy {
		t.Fatalf("observational resolution changed the group journal: head=%+v migrated=%t err=%v", head, migrated, err)
	}
}

func TestGenerationResolutionRejectsDisconnectedGroup(t *testing.T) {
	store := newGenerationStore(t)
	legacy := generationBinding(store.Root, "legacy")
	active := generationBinding(store.Root, "active")
	foreign := generationBinding(store.Root, "foreign")
	writeGenerationGroup(t, store, legacy)
	resolve := func(_ context.Context, _ string, saved task.SupervisorRef) (task.SupervisorRef, bool, error) {
		if saved == active {
			return active, true, nil
		}
		return foreign, true, nil
	}
	_, err := resolvePrivateSupervisorGenerationUsing(t.Context(), store, active, resolve)
	if !errors.Is(err, inspection.ErrGroupMigrationRequired) {
		t.Fatalf("disconnected group was accepted: %v", err)
	}
}

func TestGenerationResolutionPreservesHistoricalGroup(t *testing.T) {
	store := newGenerationStore(t)
	legacy := generationBinding(store.Root, "legacy")
	writeGenerationGroup(t, store, legacy)
	got, err := resolvePrivateSupervisorGenerationUsing(t.Context(), store, legacy, privateSupervisorLifecycleUnavailable)
	if err != nil || !got.Found || got.PrivateAuthority || got.Active != legacy || got.GroupCleanupDue {
		t.Fatalf("historical group fallback changed: resolution=%+v err=%v", got, err)
	}
}

func newGenerationStore(t *testing.T) *taskdir.Store {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := taskdir.InitStore(filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func generationBinding(root, label string) task.SupervisorRef {
	base := filepath.Dir(pueue.PrivateConfigPath(root))
	return task.SupervisorRef{
		ClientExecutable:     filepath.Join(base, "pueue"),
		ClientSHA256:         task.ComputeSHA256([]byte("client:" + label)),
		DaemonExecutable:     filepath.Join(base, "pueued"),
		DaemonSHA256:         task.ComputeSHA256([]byte("daemon:" + label)),
		ResolutionOS:         "linux",
		ResolutionHome:       filepath.Join(root, "home"),
		ResolutionDataLocal:  filepath.Join(root, "data-local"),
		ResolutionConfig:     filepath.Join(root, "config"),
		ResolutionRuntime:    filepath.Join(root, "runtime"),
		ResolutionUsername:   "fixture",
		ResolvedConfigSHA256: task.ComputeSHA256([]byte("resolved-config:" + label)),
		Endpoint:             "unix:" + filepath.Join(base, "run", "pueue.sock"),
		ConfigPath:           pueue.PrivateConfigPath(root),
		ConfigDigest:         task.ComputeSHA256([]byte("config:" + label)),
		ObservedVersion:      "pueue 4.0.4",
	}
}

func writeGenerationGroup(t *testing.T, store *taskdir.Store, binding task.SupervisorRef) {
	t.Helper()
	control, err := store.OpenInspectionGroup(true)
	if err != nil {
		t.Fatal(err)
	}
	created, writeErr := control.Put("request.json", InspectionGroup(store.RootID, binding))
	if err := errors.Join(writeErr, control.Close()); err != nil || !created {
		t.Fatalf("create group fixture: created=%t err=%v", created, err)
	}
}
