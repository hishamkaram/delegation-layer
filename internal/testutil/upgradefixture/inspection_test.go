package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func fixtureInspectionOperation(t *testing.T) (*taskdir.Store, *inspection.Operation) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	digest := task.ComputeSHA256([]byte("fixture config"))
	binding := task.SupervisorRef{
		ExecutableIdentity: task.ExecutableIdentityInstalledPathV1,
		ClientExecutable:   filepath.Join(workspace, "pueue"), DaemonExecutable: filepath.Join(workspace, "pueued"),
		ConfigPath: pueue.PrivateConfigPath(store.Root), ConfigDigest: digest, ResolvedConfigSHA256: digest,
		Endpoint: "unix:" + filepath.Join(workspace, "fixture.sock"), ObservedVersion: pueue.FixtureVersion,
	}
	op, err := openFixtureInspection(t.Context(), store, workspace, strings.Repeat("a", 32), filepath.Join(workspace, "helper"), filepath.Join(workspace, "sleep"), binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := op.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return store, op
}

func TestFixtureInspectionAdmissionUsesRuntimeCapabilityContract(t *testing.T) {
	store, op := fixtureInspectionOperation(t)
	saved := op.Request()
	if saved.Group != "default" || saved.Binding.DefinitionRevision != provider.RuntimeInspectionRevision || saved.Binding.CapabilitySHA256 == "" {
		t.Fatalf("fixture lost runtime capability binding: group=%q revision=%q", saved.Group, saved.Binding.DefinitionRevision)
	}
	loadedOp, err := inspection.LoadOperationContext(t.Context(), store, saved.IdentityID())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := loadedOp.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if loadedOp.Request().Binding.CapabilitySHA256 != saved.Binding.CapabilitySHA256 {
		t.Fatal("saved runtime capability digest changed")
	}
}
