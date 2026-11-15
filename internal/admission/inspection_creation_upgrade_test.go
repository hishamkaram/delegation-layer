package admission

import (
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

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
