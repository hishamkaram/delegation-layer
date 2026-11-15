package workspaces

import (
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func TestWorkspaceCreationGatesActualRuntimePreservingLogicalBinding(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	logical, runtime := supervisorfixture.WriteUpgradeGeneration(t, store)
	request := upgradeWorkspaceRequest(store, logical, time.Now().Add(time.Minute))
	operation, err := openWorkspaceOperationWithRuntime(t.Context(), store, request, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if operation.Request().Supervisor != logical || operation.Request().Supervisor == runtime {
		t.Fatal("creation rewrote immutable logical supervisor")
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	supervisorfixture.WriteUpgradeDrain(t, store, logical)
	before := supervisorfixture.SnapshotStore(t, store.Root)
	request.OperationID = strings.Repeat("8", 32)
	operation, err = openWorkspaceOperationWithRuntime(t.Context(), store, request, runtime)
	if operation != nil {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if err == nil || operation != nil {
		t.Fatal("workspace creation crossed Drain")
	}
	if !maps.Equal(before, supervisorfixture.SnapshotStore(t, store.Root)) {
		t.Fatal("Drain denial created workspace fragments")
	}
}

func TestExpiredWorkspaceCreationHasNoRuntimeAndHonorsDrain(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	logical, _ := supervisorfixture.WriteUpgradeGeneration(t, store)
	request := upgradeWorkspaceRequest(store, logical, time.Now().Add(-time.Minute))
	operation, err := openWorkspaceOperationWithRuntime(t.Context(), store, request, task.SupervisorRef{})
	if err != nil {
		t.Fatal(err)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	supervisorfixture.WriteUpgradeDrain(t, store, logical)
	before := supervisorfixture.SnapshotStore(t, store.Root)
	request.OperationID = strings.Repeat("8", 32)
	operation, err = openWorkspaceOperationWithRuntime(t.Context(), store, request, task.SupervisorRef{})
	if operation != nil {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if err == nil || operation != nil {
		t.Fatal("expired metadata crossed Drain")
	}
	if !maps.Equal(before, supervisorfixture.SnapshotStore(t, store.Root)) {
		t.Fatal("expired denial changed root")
	}
}

func upgradeWorkspaceRequest(store *taskdir.Store, logical task.SupervisorRef, deadline time.Time) workspace.OperationRequest {
	return workspace.OperationRequest{SchemaVersion: task.SchemaVersion, RootID: store.RootID, OperationID: strings.Repeat("7", 32), Kind: workspace.OperationCheckpoint, Subject: workspace.Subject{Kind: workspace.SubjectTask, ID: strings.Repeat("6", 32), SHA256: strings.Repeat("c", 64)}, Deadline: deadline.UTC().Format(time.RFC3339Nano), WorkerExecutable: "/installed/delegate-run", ExecutableIdentity: task.ExecutableIdentityInstalledPathV1, Supervisor: logical, Checkpoint: &workspace.CheckpointSelection{Kind: workspace.CheckpointOrdinary, Cwd: filepath.Join(store.Root, "project"), CwdIdentity: &workspace.DirectoryIdentity{Device: 1, Inode: 2}}}
}
