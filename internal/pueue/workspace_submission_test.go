package pueue

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

func TestStagedWorkspaceSubmissionUsesWorkspaceGuardianAndExactPermit(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	id := strings.Repeat("b", 32)
	digest := task.ComputeSHA256([]byte("workspace staged submission"))
	operation, err := workspace.OpenOperation(t.Context(), store, workspace.OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID,
		WorkspaceID: strings.Repeat("c", 32), OperationID: id,
		Kind: workspace.OperationProvision, Subject: workspace.Subject{Kind: workspace.SubjectAdmission, ID: strings.Repeat("d", 32), SHA256: digest},
		Deadline:         time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		WorkerExecutable: fake.executable, WorkerSHA256: digest, Supervisor: fake.client.Binding(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	identity := InspectionIdentity{RootID: store.RootID, TaskID: id, QueueGroup: "default"}
	launch := Launch{RunnerExecutable: fake.executable, RootPath: store.Root, Supervised: true, WorkspaceOperation: true}
	writeStagedInspectionStatusWithLaunch(t, fake, identity, 9, StateStashed, launch)
	permit, err := operation.SubmissionPermit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = fake.client.SubmitConcurrentInspectionStaged(t.Context(), permit, identity, launch, func(observation InspectionObservation) error {
		return operation.RecordReceipt(t.Context(), observation.Job.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	assertWorkspaceStagedCommand(t, fake, launch, identity)
	receipt, err := operation.Receipt(t.Context())
	if err != nil || receipt.NumericTaskID != 9 {
		t.Fatalf("exact receipt was not committed before enqueue: %+v, %v", receipt, err)
	}
}

func assertWorkspaceStagedCommand(t *testing.T, fake *fakeSupervisor, launch Launch, identity InspectionIdentity) {
	t.Helper()
	adds := stagedInvocations(t, fake, "add")
	if len(adds) != 1 || len(stagedInvocations(t, fake, "enqueue")) != 1 {
		t.Fatalf("wrong add/enqueue cardinality: %+v", readInvocations(t, fake.logPath))
	}
	separator := slices.Index(adds[0], "--")
	expected := []string{launch.RunnerExecutable, "--workspace-operation", "--guardian", "--supervised", "--root", launch.RootPath, identity.TaskID}
	if separator < 0 || !slices.Equal(adds[0][separator+1:], expected) {
		t.Fatalf("workspace runner mode lost in staged add: %+v", adds[0])
	}
	if got := stagedInspectionCommand(launch, identity); got != escapedCommand(expected...) {
		t.Fatalf("staged reconciliation expects a different command: %q", got)
	}
}
