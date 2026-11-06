package admission

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func inspectionHistoryDefinition() commonprovider.InspectionDefinition {
	return commonprovider.InspectionDefinition{
		Revision: "inspection-v1", Executable: "/installed/helper", Arguments: []string{"inspect"},
		Directory: "/workspace", Environment: []string{"LANG=C"}, OutputLimit: 1024,
		Project: func(data []byte) (json.RawMessage, error) { return json.RawMessage(data), nil },
	}
}

func inspectionHistorySupervisor() task.SupervisorRef {
	return task.SupervisorRef{
		ClientExecutable: "/installed/pueue", ClientSHA256: strings.Repeat("a", 64),
		ConfigPath: "/state/pueue.yml", ConfigDigest: strings.Repeat("b", 64),
		ResolvedConfigSHA256: strings.Repeat("c", 64), Endpoint: "unix:/state/socket", ObservedVersion: "pueue 4.0.4",
	}
}

func TestInstalledInspectionWriterOmitsProgramDigests(t *testing.T) {
	definition, digest, err := inspectionHistoryDefinition().Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	binding := installedInspectionBinding(InspectionOptions{RunnerOwnership: task.RunnerOwnershipManaged}, definition, digest, "", "/installed/delegate-run", inspectionHistorySupervisor())
	if err = inspection.ValidateBinding(binding); err != nil {
		t.Fatal(err)
	}
	encoded, err := task.MarshalCanonical(binding)
	if err != nil {
		t.Fatal(err)
	}
	if binding.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 || bytes.Contains(encoded, []byte(`"helper_sha256"`)) || bytes.Contains(encoded, []byte(`"worker_sha256"`)) {
		t.Fatalf("fresh writer emitted program identity digests: %s", encoded)
	}
}

func TestInstalledInspectionReplayPreservesLegacyDefinitionAndRequest(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	request := supervisorfixture.NewInspectionRequest(t, store)
	definition := inspectionHistoryDefinition()
	definition.ExecutableSHA256 = strings.Repeat("d", 64)
	_, digest, err := definition.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	saved := inspection.Binding{
		DefinitionRevision: definition.Revision, DefinitionSHA256: digest,
		HelperExecutable: definition.Executable, HelperSHA256: definition.ExecutableSHA256,
		WorkerExecutable: "/installed/delegate-run", WorkerSHA256: strings.Repeat("e", 64),
		RunnerOwnership: task.RunnerOwnershipManaged, WorkerExitProof: inspection.WorkerExitProofGuardianWaitV1,
		Environment: []string{"LANG=C"}, EnvironmentRecorded: true, Supervisor: inspectionHistorySupervisor(),
	}
	operation, err := inspection.OpenOperation(store, request, saved, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	before, err := task.MarshalCanonical(operation.Request())
	if err != nil {
		t.Fatal(err)
	}
	definition.ExecutableSHA256 = ""
	_, currentDigest, err := definition.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	current := installedInspectionBinding(InspectionOptions{RunnerOwnership: task.RunnerOwnershipManaged}, definition, currentDigest, "", saved.WorkerExecutable, saved.Supervisor)
	candidate := commonprovider.ProfileCandidate{Inspection: &definition}
	projected, err := inspectionReplayBinding(t.Context(), store, request, candidate, current)
	if err != nil {
		t.Fatalf("historical definition data was not preserved: %v", err)
	}
	replayed, err := OpenInspectionOperationContext(t.Context(), store, request, projected, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	after, encodeErr := task.MarshalCanonical(replayed.Request())
	if closeErr := replayed.Close(); closeErr != nil {
		t.Error(closeErr)
	}
	if encodeErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("legacy canonical request changed: %v", encodeErr)
	}
	definition.ExecutableSHA256 = strings.Repeat("f", 64)
	if _, err = inspectionReplayBinding(t.Context(), store, request, candidate, current); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("changed explicit historical metadata was ignored: %v", err)
	}
	definition.ExecutableSHA256 = ""
	definition.Arguments = []string{"changed-contract"}
	if _, err = inspectionReplayBinding(t.Context(), store, request, candidate, current); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("changed helper contract was reused: %v", err)
	}
}
