package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestRunInspectionWorkerRejectsInvalidTaskBeforeOpeningState(t *testing.T) {
	called := false
	got := runInspectionWorker(filepath.Join(canonicalAppTestTempDir(t), "missing"), "invalid", Dependencies{
		PrepareCandidate: func(task.TaskRecord) (commonprovider.ProfileCandidate, error) {
			called = true
			return commonprovider.ProfileCandidate{}, nil
		},
	})
	if !errors.Is(got, errInspectionWorkerUnavailable) {
		t.Fatalf("error=%v, want fixed worker error", got)
	}
	if called {
		t.Fatal("candidate preparation ran for an invalid task ID")
	}
}

func TestRunInspectionWorkerNeverCreatesMissingStore(t *testing.T) {
	root := filepath.Join(canonicalAppTestTempDir(t), "missing-state")
	got := runInspectionWorker(root, strings.Repeat("a", 32), Dependencies{})
	if !errors.Is(got, errInspectionWorkerUnavailable) {
		t.Fatalf("error=%v, want fixed worker error", got)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worker created or changed missing store %q: %v", root, err)
	}
}

func TestValidateInspectionWorkerExecutableRejectsWrongExecutable(t *testing.T) {
	_, operation := newInspectionWorkerOperation(t, time.Unix(100, 0).UTC())
	binding := operation.Request().Binding
	if err := validateInspectionWorkerExecutable(context.Background(), canonicalAppTestTempDir(t), operation, binding, filepath.Join(canonicalAppTestTempDir(t), "current-worker")); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("wrong worker executable was accepted: %v", err)
	}
}

func TestValidateInspectionWorkerExecutableRequiresStateRunnerForUpgrade(t *testing.T) {
	t.Run("authorized content-addressed state runner", func(t *testing.T) {
		store, operation := newInspectionWorkerOperationWithRunnerOwnership(t, time.Unix(100, 0).UTC(), task.RunnerOwnershipManaged)
		source := canonicalAppTestExecutable(t)
		runner := installStateRunnerFixture(t, store.Root, source)
		if err := operation.AuthorizeManagedWorkerUpgradeContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		binding := operation.Request().Binding
		if err := validateInspectionWorkerExecutable(context.Background(), store.Root, operation, binding, runner); err != nil {
			t.Fatalf("verified state-root worker upgrade was rejected: %v", err)
		}
	})

	t.Run("authorized install runner", func(t *testing.T) {
		store, operation := newInspectionWorkerOperationWithRunnerOwnership(t, time.Unix(100, 0).UTC(), task.RunnerOwnershipManaged)
		runner := canonicalAppTestExecutable(t)
		if err := operation.AuthorizeManagedWorkerUpgradeContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		binding := operation.Request().Binding
		if err := validateInspectionWorkerExecutable(context.Background(), store.Root, operation, binding, runner); !errors.Is(err, task.ErrEvidenceFault) {
			t.Fatalf("authorized install runner was accepted for task upgrade: %v", err)
		}
	})
}

func TestValidateInspectionWorkerExecutableRejectsUnmanagedUpgradeMarker(t *testing.T) {
	for _, ownership := range []string{"", task.RunnerOwnershipCustom} {
		name := map[string]string{"": "unknown", task.RunnerOwnershipCustom: "custom"}[ownership]
		t.Run(name, func(t *testing.T) {
			store, operation := newInspectionWorkerOperationWithRunnerOwnership(t, time.Unix(100, 0).UTC(), ownership)
			source := canonicalAppTestExecutable(t)
			runner := installStateRunnerFixture(t, store.Root, source)
			control, err := store.OpenInspection(operation.Request().TaskID, false)
			if err != nil {
				t.Fatal(err)
			}
			_, err = control.Put("worker-upgrade.json", inspection.ManagedWorkerUpgradeRecord{
				SchemaVersion: task.SchemaVersion,
				RequestSHA256: operation.Digest(),
			})
			err = errors.Join(err, control.Close())
			if err != nil {
				t.Fatal(err)
			}
			binding := operation.Request().Binding
			if err = validateInspectionWorkerExecutable(context.Background(), store.Root, operation, binding, runner); !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("state-root successor accepted unmanaged ownership %q: %v", ownership, err)
			}
		})
	}
}

func TestInspectionBudgetStopperRejectsClosedOperation(t *testing.T) {
	_, operation := newInspectionWorkerOperation(t, time.Unix(100, 0).UTC())
	if err := operation.Close(); err != nil {
		t.Fatal(err)
	}
	stopper := &inspectionBudgetStopper{operation: operation, client: &pueue.Client{}}
	if request, err := stopper.PrepareBudget(operation.Deadline()); request != nil || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("closed operation yielded stop authority: request=%v err=%v", request, err)
	}
}

func TestCompleteInspectionFailureSealsExpiredResult(t *testing.T) {
	_, operation := newInspectionWorkerOperation(t, time.Unix(100, 0).UTC())
	permit, err := operation.ClaimStart(time.Unix(101, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if got := completeInspectionFailure(operation); !errors.Is(got, errInspectionWorkerUnavailable) {
		t.Fatalf("error=%v, want fixed worker error", got)
	}
	result, _, err := operation.ReadCompleted()
	if err != nil {
		t.Fatal(err)
	}
	if result.Reason != inspection.ResultExpired || len(result.Facts) != 0 {
		t.Fatalf("expired result=%+v", result)
	}
}

func TestRecordInspectionStopSeparatesAcknowledgmentFromEndedObservation(t *testing.T) {
	store, operation := newInspectionWorkerOperation(t, time.Unix(100, 0).UTC())
	if err := operation.RecordReceipt(42); err != nil {
		t.Fatal(err)
	}
	if _, _, err := operation.ClaimStop(operation.Deadline()); err != nil {
		t.Fatal(err)
	}
	id := int64(42)
	acknowledged := true
	if err := recordInspectionStop(context.Background(), operation, pueue.StopResult{NumericTaskID: &id, Action: "kill", Acknowledged: &acknowledged, ObservedState: pueue.StateRunning}); err != nil {
		t.Fatal(err)
	}
	var reply inspection.StopReplyRecord
	readInspectionWorkerRecord(t, store, operation.Request().TaskID, "stop-reply.json", &reply)
	if reply.NumericTaskID != id || reply.Action != "kill" || !reply.Acknowledged {
		t.Fatalf("unexpected stop reply: %+v", reply)
	}
	if err := recordInspectionStop(context.Background(), operation, pueue.StopResult{NumericTaskID: &id, ObservedState: pueue.StateEnded}); err != nil {
		t.Fatal(err)
	}
	var observation inspection.StopObservationRecord
	readInspectionWorkerRecord(t, store, operation.Request().TaskID, "stop-observation.json", &observation)
	if observation.NumericTaskID != id || observation.State != "ended" {
		t.Fatalf("unexpected stop observation: %+v", observation)
	}
}

func readInspectionWorkerRecord(t *testing.T, store *taskdir.Store, taskID, name string, record any) {
	t.Helper()
	control, err := store.OpenInspection(taskID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := control.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if err = control.Read(name, record); err != nil {
		t.Fatal(err)
	}
}

func newInspectionWorkerOperation(t *testing.T, created time.Time) (*taskdir.Store, *inspection.Operation) {
	return newInspectionWorkerOperationWithRunnerOwnership(t, created, "")
}

func newInspectionWorkerOperationWithRunnerOwnership(t *testing.T, created time.Time, runnerOwnership string) (*taskdir.Store, *inspection.Operation) {
	t.Helper()
	stateRoot := filepath.Join(canonicalAppTestTempDir(t), "state")
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
