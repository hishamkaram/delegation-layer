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

var unavailableRuntimeOptions = OperationOptions{Runtime: unavailableOperationRuntime{}}

func observeRemoval(t *testing.T, store *taskdir.Store, original *workspace.Operation, operationID string) (CheckpointDescription, *CheckpointRemovalDescription, error) {
	t.Helper()
	return ObserveCheckpointRemoval(t.Context(), store, unavailableRuntimeOptions, original.Request().OperationID, operationID)
}

func requireRemovalDescription(t *testing.T, label string, got *CheckpointRemovalDescription, want CheckpointRemovalDescription) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s removal = %+v, want %+v", label, got, want)
	}
}

// requireRefusedObservation asserts a fault before any current metadata.
func requireRefusedObservation(t *testing.T, current CheckpointDescription, err error, cause error) {
	t.Helper()
	if current.ID != "" || err == nil || PendingError(err) || (cause != nil && !errors.Is(err, cause)) {
		t.Fatalf("observation = %+v, %v; want a concrete %v fault and no current metadata", current, err, cause)
	}
}

func TestObserveCheckpointRemovalHistoricalAttemptReadsCommittedRecordsOnly(t *testing.T) {
	cases := map[string]struct {
		deadline time.Duration
		closed   bool
		state    string
	}{
		"expired pending head": {-time.Second, false, CheckpointPending},
		"removed head":         {time.Hour, true, workspace.CheckpointRemoved},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			history := newRemovalHistory(t, priorUnavailable, time.Now().Add(test.deadline))
			if test.closed {
				closeCheckpointLifetime(t, history.store, history.head, checkpointNative, func() { recordCheckpointWork(t, history.head, nil) })
				if err := FinalizeCheckpoint(t.Context(), history.store, history.head); err != nil {
					t.Fatal(err)
				}
			}
			current, selected, err := observeRemoval(t, history.store, history.original, priorRemovalID)
			if err != nil || current.Usable {
				t.Fatalf("historical observation = %+v, %v", current, err)
			}
			requireRemovalDescription(t, "selected", selected, CheckpointRemovalDescription{OperationID: priorRemovalID, Attempts: 2, State: workspace.CheckpointUnavailable, ReasonCode: workspace.CheckpointRemovalNotLaunched})
			requireRemovalDescription(t, "current", current.Removal, CheckpointRemovalDescription{OperationID: headRemovalID, Attempts: 2, State: test.state})
			if !test.closed {
				history.requireHeadUntouched(t)
			}
		})
	}
}

// TestObserveCheckpointRemovalStaysBoundToSelectedHead registers B while the
// selected head A is being advanced: A alone is qualified, B is only read.
func TestObserveCheckpointRemovalStaysBoundToSelectedHead(t *testing.T) {
	store := newCheckpointStore(t)
	original := qualifiedOrdinaryCheckpoint(t, store, true)
	first := openCheckpointRemoval(t, store, original, priorRemovalID, time.Now().Add(-time.Second))
	registerCheckpointRemovalForTest(t, original, first, 0)
	later := openCheckpointRemoval(t, store, original, headRemovalID, time.Now().Add(-time.Second))
	var laterAttempt workspace.CheckpointRemovalReference
	var registerErr error
	register := func() { laterAttempt, _, registerErr = original.RegisterCheckpointRemoval(t.Context(), later, 1) }
	setStoreFault(t, store, &linkedRecordFault{name: priorRemovalID + "/" + unlaunchedFenceRecord, run: register})
	current, selected, err := observeRemoval(t, store, original, priorRemovalID)
	if registerErr != nil || laterAttempt.OperationID != headRemovalID {
		t.Fatalf("registration during advancement = %+v, %v", laterAttempt, registerErr)
	}
	if err != nil {
		t.Fatalf("observation of the selected head = %v", err)
	}
	requireRemovalDescription(t, "selected", selected, CheckpointRemovalDescription{OperationID: priorRemovalID, Attempts: 2, State: workspace.CheckpointUnavailable, ReasonCode: workspace.CheckpointRemovalNotLaunched})
	requireRemovalDescription(t, "current", current.Removal, CheckpointRemovalDescription{OperationID: headRemovalID, Attempts: 2, State: CheckpointPending})
	requireRemovalHeadUntouched(t, original, later, laterAttempt)
}

func TestObserveCheckpointRemovalSelectedHeadRuntimeFaultKeepsPendingIdentity(t *testing.T) {
	store := newCheckpointStore(t)
	original := qualifiedOrdinaryCheckpoint(t, store, true)
	registerCheckpointRemovalForTest(t, original, openCheckpointRemoval(t, store, original, priorRemovalID, time.Now().Add(time.Hour)), 0)
	current, selected, err := observeRemoval(t, store, original, priorRemovalID)
	requireRefusedObservation(t, current, err, errOperationFixtureRuntimeUnavailable)
	if !errors.Is(err, workspace.ErrPending) {
		t.Fatalf("unfinished head = %v, want pending joined with the runtime fault", err)
	}
	requireRemovalDescription(t, "selected", selected, CheckpointRemovalDescription{OperationID: priorRemovalID, State: CheckpointPending})
}

func TestObserveCheckpointRemovalReceiptReadFaultKeepsIdentity(t *testing.T) {
	store := newCheckpointStore(t)
	original := qualifiedOrdinaryCheckpoint(t, store, true)
	removal := openCheckpointRemoval(t, store, original, priorRemovalID, time.Now().Add(-time.Second))
	registerCheckpointRemovalForTest(t, original, removal, 0)
	if err := FinalizeCheckpoint(t.Context(), store, removal); err != nil {
		t.Fatal(err)
	}
	workspacefixture.WriteOperationFile(t, store, original.Request().OperationID, "removal-receipt-00000000000000000001.json", []byte("{"))
	current, selected, err := observeRemoval(t, store, original, priorRemovalID)
	requireRefusedObservation(t, current, err, task.ErrEvidenceFault)
	if workspace.OperationAbsent(err) {
		t.Fatalf("corrupt receipt = %v, want its decode fault, not absence", err)
	}
	requireRemovalDescription(t, "selected", selected, CheckpointRemovalDescription{OperationID: priorRemovalID})
}

func TestObserveCheckpointRemovalRefusesUnauthenticatedSelections(t *testing.T) {
	cases := map[string]struct {
		selection func(*testing.T, removalHistory) string
		cause     error
	}{
		"unregistered": {func(t *testing.T, h removalHistory) string {
			return openCheckpointRemoval(t, h.store, h.original, strings.Repeat("4", 32), time.Now().Add(-time.Second)).Request().OperationID
		}, task.ErrIdentityMismatch},
		"creation": {func(_ *testing.T, h removalHistory) string { return h.original.Request().OperationID }, task.ErrIdentityMismatch},
		"foreign checkpoint": {func(t *testing.T, h removalHistory) string {
			other := openOrdinaryCheckpoint(t, h.store, strings.Repeat("8", 32), time.Now().Add(time.Hour))
			foreign := openCheckpointRemoval(t, h.store, other, strings.Repeat("9", 32), time.Now().Add(-time.Second))
			return registerCheckpointRemovalForTest(t, other, foreign, 0).OperationID
		}, task.ErrIdentityMismatch},
		"digest mismatch": {func(t *testing.T, h removalHistory) string {
			request := workspacefixture.CheckpointOperationRequest(h.store, headRemovalID, workspace.OperationCheckpointRemove, "", checkpointRemovalSubject(h.original), nil, time.Now().Add(-time.Minute))
			data, err := task.MarshalCanonical(request)
			if err != nil {
				t.Fatal(err)
			}
			workspacefixture.WriteOperationFile(t, h.store, headRemovalID, "request.json", data)
			return headRemovalID
		}, task.ErrIdentityMismatch},
		"unknown": {func(*testing.T, removalHistory) string { return strings.Repeat("a", 32) }, nil},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			history := newRemovalHistory(t, priorUnavailable, time.Now().Add(-time.Second))
			current, selected, err := observeRemoval(t, history.store, history.original, test.selection(t, history))
			requireRefusedObservation(t, current, err, test.cause)
			if selected != nil {
				t.Fatalf("unauthenticated selection was described: %+v", selected)
			}
			history.requireHeadUntouched(t)
		})
	}
}

func TestObserveCheckpointRemovalRefusesInvalidHistoryBeforeEffects(t *testing.T) {
	for _, prior := range []string{priorReceiptMissing, priorRemoved, priorNothingToRemove} {
		for _, operationID := range []string{headRemovalID, priorRemovalID} {
			t.Run(prior+"/"+operationID[:1], func(t *testing.T) {
				history := newRemovalHistory(t, prior, time.Now().Add(-time.Second))
				current, selected, err := observeRemoval(t, history.store, history.original, operationID)
				requireRefusedObservation(t, current, err, task.ErrEvidenceFault)
				requireRemovalDescription(t, "selected", selected, CheckpointRemovalDescription{OperationID: operationID})
				history.requireHeadUntouched(t)
			})
		}
	}
}

// readValidatedRemovalInventory is the inventory an observer reads and
// validates before it advances that inventory's head.
func readValidatedRemovalInventory(t *testing.T, original *workspace.Operation) []workspace.CheckpointRemovalReference {
	t.Helper()
	attempts, err := original.CheckpointRemovalAttempts(t.Context())
	if err == nil {
		err = requireCheckpointRemovalHistory(t.Context(), original, attempts)
	}
	if err != nil {
		t.Fatal(err)
	}
	return attempts
}

// TestAdvanceCheckpointRemovalHeadHonorsItsSuppliedInventory registers B
// after inventory [A] was read: advancing [A] returns A's immutable receipt
// and leaves B untouched, while advancing a fresh [A,B] advances B.
func TestAdvanceCheckpointRemovalHeadHonorsItsSuppliedInventory(t *testing.T) {
	store := newCheckpointStore(t)
	original := qualifiedOrdinaryCheckpoint(t, store, true)
	first := openCheckpointRemoval(t, store, original, priorRemovalID, time.Now().Add(-time.Second))
	registerCheckpointRemovalForTest(t, original, first, 0)
	if err := FinalizeCheckpoint(t.Context(), store, first); err != nil {
		t.Fatal(err)
	}
	saved := readValidatedRemovalInventory(t, original)
	later := openCheckpointRemoval(t, store, original, headRemovalID, time.Now().Add(-time.Second))
	laterAttempt := registerCheckpointRemovalForTest(t, original, later, 1)
	winner, err := original.CheckpointRemovalReceipt(t.Context(), saved[0])
	if err != nil || winner.Status != workspace.CheckpointUnavailable {
		t.Fatalf("finalized first attempt = %+v, %v", winner, err)
	}
	receipt, err := advanceCheckpointRemovalHead(t.Context(), store, original, unavailableRuntimeOptions, saved)
	if err != nil || receipt == nil || *receipt != winner {
		t.Fatalf("advancing saved [A] = %+v, %v; want A's stored receipt %+v", receipt, err, winner)
	}
	requireRemovalHeadUntouched(t, original, later, laterAttempt)
	if current, describeErr := describeCheckpoint(t.Context(), original); describeErr != nil || current.Removal == nil || current.Removal.OperationID != headRemovalID || current.Removal.State != CheckpointPending {
		t.Fatalf("fresh metadata after saved advancement = %+v, %v", current, describeErr)
	}
	receipt, err = advanceCheckpointRemovalHead(t.Context(), store, original, unavailableRuntimeOptions, readValidatedRemovalInventory(t, original))
	if err != nil || receipt == nil || receipt.Attempt != laterAttempt || receipt.ReasonCode != workspace.CheckpointRemovalNotLaunched {
		t.Fatalf("advancing fresh [A,B] = %+v, %v; want B qualified", receipt, err)
	}
}
