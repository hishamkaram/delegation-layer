package admission

import (
	"errors"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInspectionCreationClosesOperationAfterGenerationCleanupFault(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	logical, runtime := supervisorfixture.WriteUpgradeGeneration(t, store)
	request := supervisorfixture.NewInspectionRequest(t, store)
	binding := inspection.Binding{DefinitionRevision: commonprovider.RuntimeInspectionRevision, DefinitionSHA256: strings.Repeat("c", 64), CapabilitySHA256: strings.Repeat("d", 64), HelperExecutable: "/installed/helper", WorkerExecutable: "/installed/delegate-run", ExecutableIdentity: task.ExecutableIdentityInstalledPathV1, RunnerOwnership: task.RunnerOwnershipManaged, WorkerExitProof: inspection.WorkerExitProofGuardianWaitV1, EnvironmentRecorded: true, Supervisor: logical}
	operation, err := createInspectionOperationWithRuntime(t.Context(), store, request, binding, runtime, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if _, _, readErr := operation.ReadCompletedContext(t.Context()); !errors.Is(readErr, os.ErrNotExist) {
		t.Fatalf("successful creation did not leave an open journal: %v", readErr)
	}

	// Model the lease finalizer's error after the real creation callback has
	// succeeded. No native lease-release failure is injected by this fixture.
	cleanupOwner := &pueue.BootstrapCleanupError{}
	releaseFault := errors.New("generation lease release failed after creation")
	returned, resultErr := finishInspectionCreation(operation, errors.Join(cleanupOwner, releaseFault))
	if returned != nil {
		t.Error("failed admission returned the created operation")
	}
	var retained *pueue.BootstrapCleanupError
	if !errors.As(resultErr, &retained) || retained != cleanupOwner || !errors.Is(resultErr, releaseFault) {
		t.Errorf("creation finalizer lost original cleanup error graph: %v", resultErr)
	}
	if _, _, readErr := operation.ReadCompletedContext(t.Context()); !errors.Is(readErr, os.ErrClosed) {
		t.Errorf("failed admission left the real journal handle open: %v", readErr)
	}
	if reopened, reopenErr := LoadInspectionOperationContext(t.Context(), store, request); reopenErr != nil {
		t.Errorf("cleanup destroyed the durable inspection request: %v", reopenErr)
	} else if closeErr := reopened.Close(); closeErr != nil {
		t.Error(closeErr)
	}
}

func TestInspectionCreationGatesActualRuntimePreservingLogicalBinding(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	logical, runtime := supervisorfixture.WriteUpgradeGeneration(t, store)
	request := supervisorfixture.NewInspectionRequest(t, store)
	binding := inspection.Binding{DefinitionRevision: commonprovider.RuntimeInspectionRevision, DefinitionSHA256: strings.Repeat("c", 64), CapabilitySHA256: strings.Repeat("d", 64), HelperExecutable: "/installed/helper", WorkerExecutable: "/installed/delegate-run", ExecutableIdentity: task.ExecutableIdentityInstalledPathV1, RunnerOwnership: task.RunnerOwnershipManaged, WorkerExitProof: inspection.WorkerExitProofGuardianWaitV1, EnvironmentRecorded: true, Supervisor: logical}
	operation, err := createInspectionOperationWithRuntime(t.Context(), store, request, binding, runtime, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if operation.Request().Binding.Supervisor != logical || operation.Request().Binding.Supervisor == runtime {
		t.Fatal("creation rewrote immutable logical supervisor")
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}
	supervisorfixture.WriteUpgradeDrain(t, store, logical)
	before := supervisorfixture.SnapshotStore(t, store.Root)
	request.TaskID = strings.Repeat("8", 32)
	blocked, err := createInspectionOperationWithRuntime(t.Context(), store, request, binding, runtime, time.Now().UTC())
	if blocked != nil {
		if closeErr := blocked.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if err == nil || blocked != nil {
		t.Fatal("inspection creation crossed Drain")
	}
	if !maps.Equal(before, supervisorfixture.SnapshotStore(t, store.Root)) {
		t.Fatal("Drain denial created inspection fragments")
	}
}
