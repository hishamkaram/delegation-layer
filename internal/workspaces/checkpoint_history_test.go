package workspaces

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

// Predecessor dispositions of a two-attempt removal history. Only a stored
// unavailable receipt lets a later attempt register.
const (
	priorUnavailable     = workspace.CheckpointUnavailable
	priorReceiptMissing  = "missing"
	priorRemoved         = workspace.CheckpointRemoved
	priorNothingToRemove = workspace.CheckpointNothingToRemove
)

var (
	priorRemovalID = strings.Repeat("2", 32)
	headRemovalID  = strings.Repeat("3", 32)
)

// removalHistory is a qualified ordinary checkpoint with a predecessor removal
// attempt and a registered head attempt.
type removalHistory struct {
	store      *taskdir.Store
	original   *workspace.Operation
	head       *workspace.Operation
	attempt    workspace.CheckpointRemovalReference
	operations []string
}

// newRemovalHistory stores prior as the first attempt's disposition through
// the production finalizer, then registers a receipt-less head attempt.
func newRemovalHistory(t *testing.T, prior string, headDeadline time.Time) removalHistory {
	t.Helper()
	store := newCheckpointStore(t)
	original := qualifiedOrdinaryCheckpoint(t, store, prior != priorNothingToRemove)
	deadline := time.Now().Add(time.Hour)
	if prior == priorUnavailable {
		deadline = time.Now().Add(-time.Second)
	}
	first := openCheckpointRemoval(t, store, original, priorRemovalID, deadline)
	registerCheckpointRemovalForTest(t, original, first, 0)
	if prior == priorRemoved || prior == priorNothingToRemove {
		closeCheckpointLifetime(t, store, first, checkpointNative, func() { recordCheckpointWork(t, first, nil) })
	}
	if prior != priorReceiptMissing {
		if err := FinalizeCheckpoint(t.Context(), store, first); err != nil {
			t.Fatal(err)
		}
	}
	head := openCheckpointRemoval(t, store, original, headRemovalID, headDeadline)
	attempt := registerCheckpointRemovalForTest(t, original, head, 1)
	operations, err := store.WorkspaceOperationIDs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return removalHistory{store: store, original: original, head: head, attempt: attempt, operations: operations}
}

// requireRemovalHeadUntouched asserts that no reader or refused action
// fenced, qualified or submitted the head attempt.
func requireRemovalHeadUntouched(t *testing.T, original, head *workspace.Operation, attempt workspace.CheckpointRemovalReference) {
	t.Helper()
	if _, err := head.ReadLifetimeUnlaunchedContext(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("head lifetime was fenced: %v", err)
	}
	if _, err := original.CheckpointRemovalReceipt(t.Context(), attempt); !workspace.OperationAbsent(err) {
		t.Fatalf("head received a removal receipt: %v", err)
	}
	if _, err := head.Receipt(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("head was submitted: %v", err)
	}
}

func (h removalHistory) requireHeadUntouched(t *testing.T) {
	t.Helper()
	requireRemovalHeadUntouched(t, h.original, h.head, h.attempt)
}

// requireNoNewRemoval asserts the history still has its two registered
// attempts and the store gained no removal request.
func (h removalHistory) requireNoNewRemoval(t *testing.T) {
	t.Helper()
	attempts, err := h.original.CheckpointRemovalAttempts(t.Context())
	if err != nil || len(attempts) != 2 || attempts[1] != h.attempt {
		t.Fatalf("attempts = %+v, %v; want the same two", attempts, err)
	}
	if ids, idsErr := h.store.WorkspaceOperationIDs(t.Context()); idsErr != nil || len(ids) != len(h.operations) {
		t.Fatalf("operations = %v, %v; want no new removal request", ids, idsErr)
	}
}

func requireConcreteFault(t *testing.T, consumer string, err error, cause error) {
	t.Helper()
	if !errors.Is(err, cause) || PendingError(err) {
		t.Fatalf("%s = %v; want concrete %v, not pending", consumer, err, cause)
	}
}

// requireReadersFault pairs metadata description, listing and observation
// over one history; none may return partial data.
func (h removalHistory) requireReadersFault(t *testing.T, cause error) {
	t.Helper()
	id := h.original.Request().OperationID
	described, err := describeCheckpoint(t.Context(), h.original)
	requireConcreteFault(t, "describe", err, cause)
	page, cursor, err := ListCheckpoints(t.Context(), h.store, "", 10)
	requireConcreteFault(t, "list", err, cause)
	if page != nil || cursor != "" {
		t.Fatalf("list returned partial data %+v, cursor %q", page, cursor)
	}
	observed, err := ObserveCheckpoint(t.Context(), h.store, OperationOptions{Runtime: unavailableOperationRuntime{}}, id)
	requireConcreteFault(t, "observe", err, cause)
	if described.ID != "" || observed.ID != "" {
		t.Fatalf("faulted readers returned descriptions %+v, %+v", described, observed)
	}
}

// requireBeginFault asserts the action keeps its authorized head
// acknowledgement with the fault and registers or submits nothing.
func (h removalHistory) requireBeginFault(t *testing.T, cause error) {
	t.Helper()
	deps := OperationOptions{Runtime: unavailableOperationRuntime{}}
	ack, err := BeginCheckpointRemoval(t.Context(), h.store, deps, h.original.Request().OperationID, time.Minute)
	requireConcreteFault(t, "begin", err, cause)
	if !ack.Authorized() || ack.OperationID() != h.attempt.OperationID {
		t.Fatalf("begin acknowledgement = %+v; want the authorized head %s", ack, h.attempt.OperationID)
	}
	h.requireNoNewRemoval(t)
	h.requireHeadUntouched(t)
}

func TestCheckpointRemovalHistoryFaultsEveryReaderAndAction(t *testing.T) {
	for _, prior := range []string{priorReceiptMissing, priorRemoved, priorNothingToRemove} {
		t.Run(prior, func(t *testing.T) {
			history := newRemovalHistory(t, prior, time.Now().Add(-time.Second))
			history.requireReadersFault(t, task.ErrEvidenceFault)
			history.requireHeadUntouched(t)
			history.requireBeginFault(t, task.ErrEvidenceFault)
		})
	}
}

func TestCheckpointRemovalHistoryAuthenticatesEveryRequestBeforeRankingHead(t *testing.T) {
	cases := map[string]struct {
		corrupt func(*testing.T, *taskdir.Store) []byte
		cause   error
	}{
		"malformed": {func(*testing.T, *taskdir.Store) []byte { return []byte("{") }, nil},
		"foreign": {func(t *testing.T, store *taskdir.Store) []byte {
			foreign := workspace.Subject{Kind: workspace.SubjectOperation, ID: strings.Repeat("1", 32), SHA256: task.ComputeSHA256([]byte("foreign"))}
			request := workspacefixture.CheckpointOperationRequest(store, priorRemovalID, workspace.OperationCheckpointRemove, "", foreign, nil, time.Now().Add(time.Hour))
			data, err := task.MarshalCanonical(request)
			if err != nil {
				t.Fatal(err)
			}
			return data
		}, task.ErrIdentityMismatch},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			history := newRemovalHistory(t, priorUnavailable, time.Now().Add(-time.Second))
			workspacefixture.WriteOperationFile(t, history.store, priorRemovalID, "request.json", test.corrupt(t, history.store))
			// Metadata reads records only and never loads a removal request.
			if described, err := describeCheckpoint(t.Context(), history.original); err != nil || described.Removal == nil || described.Removal.Attempts != 2 {
				t.Fatalf("metadata over a valid prefix = %+v, %v", described, err)
			}
			ack, err := BeginCheckpointRemoval(t.Context(), history.store, OperationOptions{Runtime: unavailableOperationRuntime{}}, history.original.Request().OperationID, time.Minute)
			if err == nil || PendingError(err) || workspace.OperationAbsent(err) || (test.cause != nil && !errors.Is(err, test.cause)) ||
				!ack.Authorized() || ack.OperationID() != history.attempt.OperationID {
				t.Fatalf("begin over an unauthenticated predecessor = %+v, %v", ack, err)
			}
			history.requireNoNewRemoval(t)
			history.requireHeadUntouched(t)
		})
	}
}

// TestCheckpointRemovalHistoryAcceptsUnavailablePrefix keeps the legitimate
// histories readable: metadata writes nothing even for an expired head, and
// an unfinished head stays pending without becoming a success.
func TestCheckpointRemovalHistoryAcceptsUnavailablePrefix(t *testing.T) {
	cases := map[string]struct {
		prior    string
		deadline time.Duration
		attempts uint64
	}{
		"sole pending head":         {"", time.Hour, 1},
		"pending head after prefix": {priorUnavailable, time.Hour, 2},
		"expired head after prefix": {priorUnavailable, -time.Second, 2},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			store, original, head, attempt := pendingRemovalHistory(t, test.prior, time.Now().Add(test.deadline))
			page, cursor, err := ListCheckpoints(t.Context(), store, "", 10)
			if err != nil || cursor != "" || len(page) != 1 || page[0].Usable || page[0].Removal == nil ||
				page[0].Removal.State != CheckpointPending || page[0].Removal.Attempts != test.attempts {
				t.Fatalf("listed history = %+v, %q, %v", page, cursor, err)
			}
			requireRemovalHeadUntouched(t, original, head, attempt)
			if test.deadline < 0 {
				return
			}
			reconciled, err := reconcileCheckpointRemoval(t.Context(), store, original, OperationOptions{Runtime: unavailableOperationRuntime{}})
			if !errors.Is(err, workspace.ErrPending) || reconciled.receipt != nil || reconciled.attempt != attempt {
				t.Fatalf("unfinished head = %+v, %v; want pending", reconciled, err)
			}
			if _, err = original.CheckpointRemovalReceipt(t.Context(), attempt); !workspace.OperationAbsent(err) {
				t.Fatalf("unfinished head was qualified: %v", err)
			}
		})
	}
}

// pendingRemovalHistory registers a receipt-less head, after an unavailable
// predecessor when prior names one.
func pendingRemovalHistory(t *testing.T, prior string, deadline time.Time) (*taskdir.Store, *workspace.Operation, *workspace.Operation, workspace.CheckpointRemovalReference) {
	t.Helper()
	if prior != "" {
		history := newRemovalHistory(t, prior, deadline)
		return history.store, history.original, history.head, history.attempt
	}
	store := newCheckpointStore(t)
	original := qualifiedOrdinaryCheckpoint(t, store, true)
	head := openCheckpointRemoval(t, store, original, headRemovalID, deadline)
	return store, original, head, registerCheckpointRemovalForTest(t, original, head, 0)
}

func TestCheckpointRemovalHistoryAcceptsSuccessfulHeadAfterPrefix(t *testing.T) {
	history := newRemovalHistory(t, priorUnavailable, time.Now().Add(time.Hour))
	closeCheckpointLifetime(t, history.store, history.head, checkpointNative, func() { recordCheckpointWork(t, history.head, nil) })
	if err := FinalizeCheckpoint(t.Context(), history.store, history.head); err != nil {
		t.Fatal(err)
	}
	page, _, err := ListCheckpoints(t.Context(), history.store, "", 10)
	if err != nil || len(page) != 1 || page[0].Removal == nil || page[0].Removal.State != workspace.CheckpointRemoved || page[0].Removal.Attempts != 2 {
		t.Fatalf("listed removed history = %+v, %v", page, err)
	}
	head, err := reconcileCheckpointRemoval(t.Context(), history.store, history.original, OperationOptions{})
	if err != nil || head.receipt == nil || head.receipt.Status != workspace.CheckpointRemoved {
		t.Fatalf("reconciled removed history = %+v, %v", head, err)
	}
}

func TestCheckpointRemovalHistoryKeepsCorruptPriorReceiptConcrete(t *testing.T) {
	history := newRemovalHistory(t, priorUnavailable, time.Now().Add(-time.Second))
	workspacefixture.WriteOperationFile(t, history.store, history.original.Request().OperationID, "removal-receipt-00000000000000000001.json", []byte("{"))
	// The journal's decode fault is kept whole, never reclassified as absent.
	if _, err := describeCheckpoint(t.Context(), history.original); workspace.OperationAbsent(err) {
		t.Fatalf("corrupt predecessor receipt = %v, want the decode fault", err)
	}
	history.requireReadersFault(t, task.ErrEvidenceFault)
	history.requireBeginFault(t, task.ErrEvidenceFault)
}

func TestCheckpointRemovalHistoryReadersKeepCancellation(t *testing.T) {
	history := newRemovalHistory(t, priorUnavailable, time.Now().Add(-time.Second))
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := describeCheckpoint(cancelled, history.original)
	requireConcreteFault(t, "cancelled describe", err, context.Canceled)
	page, cursor, err := ListCheckpoints(cancelled, history.store, "", 10)
	requireConcreteFault(t, "cancelled list", err, context.Canceled)
	if page != nil || cursor != "" {
		t.Fatalf("cancelled list returned %+v, %q", page, cursor)
	}
	history.requireHeadUntouched(t)
}

// TestObservePendingCreationJoinsHistoryFault reads a managed creation whose
// helper is unfinished, so observation skips removal reconciliation; the
// description's own history check must still join the fault.
func TestObservePendingCreationJoinsHistoryFault(t *testing.T) {
	store := newCheckpointStore(t)
	selection := workspace.CheckpointSelection{Kind: workspace.CheckpointManaged}
	original := openCheckpointCreation(t, store, strings.Repeat("1", 32), strings.Repeat("b", 32), selection, time.Now().Add(time.Hour))
	first := openCheckpointRemoval(t, store, original, priorRemovalID, time.Now().Add(time.Hour))
	registerCheckpointRemovalForTest(t, original, first, 0)
	head := openCheckpointRemoval(t, store, original, headRemovalID, time.Now().Add(-time.Second))
	attempt := registerCheckpointRemovalForTest(t, original, head, 1)
	described, err := ObserveCheckpoint(t.Context(), store, OperationOptions{Runtime: unavailableOperationRuntime{}}, original.Request().OperationID)
	if !errors.Is(err, workspace.ErrPending) || !errors.Is(err, task.ErrEvidenceFault) || PendingError(err) || described.ID != "" {
		t.Fatalf("pending creation with invalid history = %+v, %v; want both causes", described, err)
	}
	requireRemovalHeadUntouched(t, original, head, attempt)
}
