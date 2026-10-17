package testsupport

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func NewInspectionWorkerOperation(t *testing.T, created time.Time) (*taskdir.Store, *inspection.Operation) {
	return NewInspectionWorkerOperationWithRunnerOwnership(t, created, "")
}

func NewInspectionWorkerOperationWithRunnerOwnership(t *testing.T, created time.Time, runnerOwnership string) (*taskdir.Store, *inspection.Operation) {
	return NewInspectionWorkerOperationWithExitProof(t, created, runnerOwnership, "")
}

func NewInspectionWorkerOperationWithExitProof(t *testing.T, created time.Time, runnerOwnership, workerExitProof string) (*taskdir.Store, *inspection.Operation) {
	t.Helper()
	stateRoot := filepath.Join(supervisorfixture.CanonicalTempDir(t), "state")
	store, err := taskdir.InitStore(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	request := task.TaskRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        store.RootID,
		TaskID:        strings.Repeat("a", 32),
		Provider:      config.ProviderFixture,
		Mode:          config.ModeReadOnly,
		CanonicalCwd:  workspace,
		RequestedConfig: task.TaskConfig{
			Permission: config.ModeReadOnly,
			Budget:     "1m0s",
		},
		BudgetNanos: int64(time.Minute),
		BriefSHA256: task.ComputeSHA256([]byte("inspection brief")),
		BriefLength: int64(len("inspection brief")),
	}
	digest := strings.Repeat("0", 64)
	binding := inspection.Binding{
		DefinitionRevision: "inspection-v1",
		DefinitionSHA256:   digest,
		HelperExecutable:   "/bin/sh",
		HelperSHA256:       digest,
		WorkerExecutable:   "/bin/sh",
		WorkerSHA256:       digest,
		RunnerOwnership:    runnerOwnership,
		WorkerExitProof:    workerExitProof,
		Supervisor: task.SupervisorRef{
			ClientExecutable:     "/bin/sh",
			ClientSHA256:         digest,
			ResolvedConfigSHA256: digest,
			Endpoint:             "/private/pueue.sock",
			ConfigPath:           filepath.Join(workspace, "pueue.yml"),
			ConfigDigest:         digest,
			ObservedVersion:      pueue.FixtureVersion,
		},
	}
	operation, err := inspection.OpenOperation(store, request, binding, created)
	if err != nil {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return store, operation
}
