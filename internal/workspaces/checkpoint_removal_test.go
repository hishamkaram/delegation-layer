package workspaces

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

// qualifiedOrdinaryCheckpoint is a closed creation with its receipt. Without a
// candidate its helper never launched, so it has nothing to remove.
func qualifiedOrdinaryCheckpoint(t *testing.T, store *taskdir.Store, withCandidate bool) *workspace.Operation {
	t.Helper()
	if !withCandidate {
		operation := openOrdinaryCheckpoint(t, store, strings.Repeat("1", 32), time.Now().Add(-time.Second))
		if receipt, err := QualifyCheckpoint(t.Context(), store, operation); err != nil || receipt.ReasonCode != workspace.CheckpointNotLaunched {
			t.Fatalf("unlaunched original = %+v, %v", receipt, err)
		}
		return operation
	}
	operation := openOrdinaryCheckpoint(t, store, strings.Repeat("1", 32), time.Now().Add(time.Hour))
	target := ordinaryCheckpointTarget()
	closeCheckpointLifetime(t, store, operation, checkpointNative, func() { recordCheckpointWork(t, operation, &target) })
	if receipt, err := QualifyCheckpoint(t.Context(), store, operation); err != nil || receipt.Status != workspace.CheckpointCheckpointed {
		t.Fatalf("native original = %+v, %v", receipt, err)
	}
	return operation
}

func openCheckpointRemoval(t *testing.T, store *taskdir.Store, original *workspace.Operation, operationID string, deadline time.Time) *workspace.Operation {
	t.Helper()
	request := workspacefixture.CheckpointOperationRequest(store, operationID, workspace.OperationCheckpointRemove, "", checkpointRemovalSubject(original), nil, deadline)
	return workspacefixture.OpenCheckpointOperation(t, store, request)
}

func registerCheckpointRemovalForTest(t *testing.T, original, removal *workspace.Operation, head uint64) workspace.CheckpointRemovalReference {
	t.Helper()
	attempt, created, err := original.RegisterCheckpointRemoval(t.Context(), removal, head)
	if err != nil || !created {
		t.Fatalf("register removal after %d = %v, %v", head, created, err)
	}
	return attempt
}

func TestCheckpointRemovalDispositionDerivesFromCandidateAndNativeClosure(t *testing.T) {
	cases := []struct {
		name          string
		withCandidate bool
		stage         string
		status        string
	}{
		{"candidate-native", true, checkpointNative, workspace.CheckpointRemoved},
		{"no-candidate-native", false, checkpointNative, workspace.CheckpointNothingToRemove},
		{"candidate-recovered", true, checkpointRegisteredRecovered, workspace.CheckpointUnavailable},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store := newCheckpointStore(t)
			original := qualifiedOrdinaryCheckpoint(t, store, test.withCandidate)
			removal := openCheckpointRemoval(t, store, original, strings.Repeat("2", 32), time.Now().Add(time.Hour))
			attempt := registerCheckpointRemovalForTest(t, original, removal, 0)
			closeCheckpointLifetime(t, store, removal, test.stage, func() { recordCheckpointWork(t, removal, nil) })
			if err := FinalizeCheckpoint(t.Context(), store, removal); err != nil {
				t.Fatal(err)
			}
			receipt, err := original.CheckpointRemovalReceipt(t.Context(), attempt)
			if err != nil || receipt.Status != test.status {
				t.Fatalf("removal receipt = %+v, %v; want %s", receipt, err, test.status)
			}
			if test.status == workspace.CheckpointUnavailable && receipt.ReasonCode != workspace.CheckpointRemovalResultUnavailable {
				t.Fatalf("recovered removal success reason = %q", receipt.ReasonCode)
			}
			description, err := describeCheckpoint(t.Context(), original)
			if err != nil || description.Usable || description.Removal == nil || description.Removal.State != test.status || description.Removal.Attempts != 1 {
				t.Fatalf("described removal = %+v, %v", description, err)
			}
		})
	}
}

func TestCheckpointRemovalLosingRequestNeverRuns(t *testing.T) {
	store := newCheckpointStore(t)
	original := qualifiedOrdinaryCheckpoint(t, store, true)
	winner := openCheckpointRemoval(t, store, original, strings.Repeat("2", 32), time.Now().Add(time.Hour))
	loser := openCheckpointRemoval(t, store, original, strings.Repeat("3", 32), time.Now().Add(time.Hour))
	attempt := registerCheckpointRemovalForTest(t, original, winner, 0)
	if _, _, err := original.RegisterCheckpointRemoval(t.Context(), loser, 0); !errors.Is(err, workspace.ErrCheckpointRemovalHeadChanged) {
		t.Fatalf("stale registration = %v, want head changed", err)
	}
	if err := ValidateCheckpointAction(t.Context(), store, winner); err != nil {
		t.Fatalf("registered winner = %v", err)
	}
	if err := ValidateCheckpointAction(t.Context(), store, loser); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("unregistered loser = %v, want refusal", err)
	}
	if err := SubmitOperation(t.Context(), store, loser, OperationOptions{Runtime: unavailableOperationRuntime{}}); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("unregistered loser submission = %v, want refusal before any runtime use", err)
	}
	if err := FinalizeCheckpoint(t.Context(), store, loser); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("unregistered loser finalization = %v, want refusal", err)
	}
	if attempts, err := original.CheckpointRemovalAttempts(t.Context()); err != nil || len(attempts) != 1 || attempts[0] != attempt {
		t.Fatalf("attempts after race = %+v, %v", attempts, err)
	}
}

func TestCheckpointRemovalHistoryRequiresEarlierNonSuccessReceipts(t *testing.T) {
	store := newCheckpointStore(t)
	original := qualifiedOrdinaryCheckpoint(t, store, true)
	expired := time.Now().Add(-time.Second)
	first := openCheckpointRemoval(t, store, original, strings.Repeat("2", 32), expired)
	registerCheckpointRemovalForTest(t, original, first, 0)
	second := openCheckpointRemoval(t, store, original, strings.Repeat("3", 32), expired)
	secondAttempt := registerCheckpointRemovalForTest(t, original, second, 1)
	if _, err := reconcileCheckpointRemoval(t.Context(), store, original, OperationOptions{Runtime: unavailableOperationRuntime{}}); !errors.Is(err, task.ErrEvidenceFault) || PendingError(err) {
		t.Fatalf("earlier attempt without a receipt = %v, want evidence fault", err)
	}
	requireRemovalHeadUntouched(t, original, second, secondAttempt)
	// A late finalization still records the earlier attempt's own receipt.
	if err := FinalizeCheckpoint(t.Context(), store, first); err != nil {
		t.Fatal(err)
	}
	head, err := reconcileCheckpointRemoval(t.Context(), store, original, OperationOptions{})
	if err != nil || head.count != 2 || head.attempt.OperationID != second.Request().OperationID || head.receipt == nil || head.receipt.ReasonCode != workspace.CheckpointRemovalNotLaunched {
		t.Fatalf("expired head = %+v, %v", head, err)
	}
}

func TestCheckpointRemovalAfterEarlierSuccessIsAnEvidenceFault(t *testing.T) {
	store := newCheckpointStore(t)
	original := qualifiedOrdinaryCheckpoint(t, store, true)
	first := openCheckpointRemoval(t, store, original, strings.Repeat("2", 32), time.Now().Add(time.Hour))
	registerCheckpointRemovalForTest(t, original, first, 0)
	closeCheckpointLifetime(t, store, first, checkpointNative, func() { recordCheckpointWork(t, first, nil) })
	if err := FinalizeCheckpoint(t.Context(), store, first); err != nil {
		t.Fatal(err)
	}
	second := openCheckpointRemoval(t, store, original, strings.Repeat("3", 32), time.Now().Add(-time.Second))
	secondAttempt := registerCheckpointRemovalForTest(t, original, second, 1)
	if _, err := reconcileCheckpointRemoval(t.Context(), store, original, OperationOptions{Runtime: unavailableOperationRuntime{}}); !errors.Is(err, task.ErrEvidenceFault) || PendingError(err) {
		t.Fatalf("attempt after a qualified success = %v, want evidence fault", err)
	}
	requireRemovalHeadUntouched(t, original, second, secondAttempt)
}

func TestCheckpointRemovalRegisteredBeforeEnqueueRedrivesSameRequest(t *testing.T) {
	store := newCheckpointStore(t)
	original := qualifiedOrdinaryCheckpoint(t, store, true)
	removal := openCheckpointRemoval(t, store, original, strings.Repeat("2", 32), time.Now().Add(time.Hour))
	attempt := registerCheckpointRemovalForTest(t, original, removal, 0)
	head, err := reconcileCheckpointRemoval(t.Context(), store, original, OperationOptions{Runtime: unavailableOperationRuntime{}})
	if !errors.Is(err, workspace.ErrPending) || !errors.Is(err, errOperationFixtureRuntimeUnavailable) {
		t.Fatalf("crashed registration = %v, want pending redrive of its own request", err)
	}
	if head.attempt != attempt || head.receipt != nil {
		t.Fatalf("redrive selected %+v", head)
	}
	if attempts, attemptsErr := original.CheckpointRemovalAttempts(t.Context()); attemptsErr != nil || len(attempts) != 1 {
		t.Fatalf("redrive registered another attempt: %+v, %v", attempts, attemptsErr)
	}
}
