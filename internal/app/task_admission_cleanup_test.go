package app

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/admission"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
)

func TestTaskCommandProjectionRetainsAdmissionCleanup(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	releaseErr := errors.New("native admission release failed")
	for _, test := range []struct {
		name  string
		build func() commandResult
		store *taskdir.Store
	}{
		{name: "dispatch", store: store, build: func() commandResult {
			return dispatchCommandResult(tasklifecycle.DispatchResult{
				Creation:     &tasklifecycle.CreationResult{Submission: &tasklifecycle.SubmissionResult{}},
				StoreCleanup: store, StoreCloseError: releaseErr,
			})
		}},
		{name: "dispatched continuation", store: store, build: func() commandResult {
			return continuationCommandResult(tasklifecycle.ContinuationResult{
				Mode: commonprovider.ContinuationNative,
				Dispatch: &tasklifecycle.DispatchResult{
					Creation:     &tasklifecycle.CreationResult{Submission: &tasklifecycle.SubmissionResult{}},
					StoreCleanup: store, StoreCloseError: releaseErr,
				},
			})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := test.build()
			var cleanup *taskAdmissionCleanupError
			if result.code != 1 || !errors.Is(result.err, releaseErr) || !errors.As(result.err, &cleanup) {
				t.Fatalf("cleanup lost in command projection: code=%d err=%v", result.code, result.err)
			}
			if cleanup.store != test.store {
				t.Fatal("command error lost the exact unresolved resource owner")
			}
			if next := result.response.Continuation; next != nil && (next.Resumable || next.ContinueCommand != "") {
				t.Fatalf("unresolved admission cleanup advertised continuation: %+v", next)
			}
		})
	}
}

func TestTaskObservationCleanupKeepsOnlyUnresolvedBorrowedStore(t *testing.T) {
	for _, kind := range []string{"repair", "workspace"} {
		t.Run(kind, func(t *testing.T) { assertObservationCleanupStore(t, kind) })
	}
}

func assertObservationCleanupStore(t *testing.T, kind string) {
	t.Helper()
	store := supervisorfixture.NewInspectionStore(t)
	other, err := taskdir.OpenStore(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	lease := appCleanupLifetimeLease(t, store)
	cleanup := lease.Release
	t.Cleanup(func() {
		store.SetFaultInjector(nil)
		if closeErr := errors.Join(cleanup(), other.Close()); closeErr != nil {
			t.Error(closeErr)
		}
	})
	fault := errors.New("observation cleanup unlock failure")
	store.SetFaultInjector(&workspaceAdmissionUnlockFault{cause: fault})
	var cleanupErr error
	if kind == "repair" {
		cleanupErr = lease.ReleaseWithError(nil)
	} else {
		owner := &workspaceCommandResources{store: store, childOwner: lease}
		cleanup = owner.close
		cleanupErr = owner.closeAfter(nil)
	}
	if !errors.Is(cleanupErr, fault) {
		t.Fatalf("native failure did not preserve cleanup ownership: %v", cleanupErr)
	}
	result := commandResult{err: cleanupErr}
	closeObservationStore(&result, store)
	if _, pinErr := store.PinRootContext(t.Context()); pinErr != nil {
		t.Fatalf("observation closed the borrowed Store behind unresolved cleanup: %v", pinErr)
	}
	closeObservationStore(&result, other)
	if _, pinErr := other.PinRootContext(t.Context()); !errors.Is(pinErr, os.ErrClosed) {
		t.Fatalf("cleanup retained a different Store for the same root: %v", pinErr)
	}
	store.SetFaultInjector(nil)
	if err = cleanup(); err != nil {
		t.Fatal(err)
	}
	closeObservationStore(&result, store)
	if _, pinErr := store.PinRootContext(t.Context()); !errors.Is(pinErr, os.ErrClosed) {
		t.Fatalf("resolved cleanup still retained the borrowed Store: %v", pinErr)
	}
}

func TestContinuedDispatchRetainsItsActualCleanupOwner(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	other, err := taskdir.OpenStore(store.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := other.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	settle, leaseFault := retainedContinuationProjectionLease(t, store, strings.Repeat("a", 32))
	cause := errors.New("supervisor refuses continuation")
	abort := errors.New("workspace abort failed")
	description := errors.New("workspace description failed")
	closeErr := errors.New("workspace close failed")
	assessment := resourceadmission.NewAssessment(time.Now(), resourceadmission.Scope{}, resourceadmission.Findings{})
	finished := workspaces.AdmissionFinish{
		Description: workspaces.Description{ID: "retained-workspace", State: "ready"},
		AbortError:  abort, DescriptionError: description, CloseError: closeErr,
	}
	dispatched := tasklifecycle.DispatchResult{
		Root: store.Root, RootID: store.RootID, TaskID: strings.Repeat("c", 32), Record: tasklifecycle.RecordAbsent,
		Phase: tasklifecycle.DispatchPredecessor, Failure: admission.WithStage("resolve-predecessor", cause),
		Workspace: &finished, Resources: &assessment, StoreCleanup: store, StoreCloseError: leaseFault,
	}
	result := continuationCommandResult(tasklifecycle.ContinuationResult{
		Root: store.Root, RootID: store.RootID, Dispatch: &dispatched,
	})
	assertContinueProjectionCleanup(t, result, []*taskdir.Store{store}, []error{cause, abort, description, closeErr, leaseFault})
	if result.response.Workspace == nil || result.response.Workspace.ID != finished.Description.ID || result.response.ResourceAdmission == nil {
		t.Fatal("Continue lost nested admission description or resource assessment")
	}
	closeObservationStore(&result, other)
	if _, pinErr := other.PinRootContext(t.Context()); !errors.Is(pinErr, os.ErrClosed) {
		t.Fatalf("cleanup retained a nonowner Store for the same root: %v", pinErr)
	}
	if _, pinErr := store.PinRootContext(t.Context()); pinErr != nil {
		t.Fatalf("closing nonowner Store closed the retained cleanup owner: %v", pinErr)
	}
	if err = settle(); err != nil {
		t.Fatal(err)
	}
	closeObservationStore(&result, store)
	if _, pinErr := store.PinRootContext(t.Context()); !errors.Is(pinErr, os.ErrClosed) {
		t.Fatalf("settled dispatch cleanup did not close its exact Store: %v", pinErr)
	}
}

func retainedContinuationProjectionLease(t *testing.T, store *taskdir.Store, id string) (func() error, error) {
	t.Helper()
	control, err := store.OpenInspectionContext(t.Context(), id, true)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := control.AcquireLifetimeOwner(t.Context())
	if err != nil {
		if closeErr := control.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	settle := func() error {
		store.SetFaultInjector(nil)
		return errors.Join(lease.Release(), control.Close())
	}
	t.Cleanup(func() {
		if closeErr := settle(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	fault := errors.New("retained projection lease " + id)
	store.SetFaultInjector(&workspaceAdmissionUnlockFault{cause: fault})
	cause := lease.ReleaseWithError(nil)
	if !errors.Is(cause, fault) || !lease.CleanupPending() {
		t.Fatalf("fixture did not retain its actual lease: %v", cause)
	}
	return settle, cause
}

func assertContinueProjectionCleanup(t *testing.T, result commandResult, stores []*taskdir.Store, causes []error) {
	t.Helper()
	if result.code != 1 || result.response.TaskID != strings.Repeat("c", 32) || result.response.TaskRecord != "not_created" {
		t.Fatalf("cleanup changed the absent successor envelope: %+v", result)
	}
	// These fixture errors have comparable identities. Count exact nodes once
	// through the canonical walker, rather than counting ancestor As matches.
	nodeCounts := make(map[error]int)
	task.AnyErrorNode(result.err, func(node error) bool {
		nodeCounts[node]++
		return false
	})
	owners := make(map[*taskdir.Store]int)
	seen := make(map[*taskAdmissionCleanupError]bool)
	for node := range nodeCounts {
		var retained *taskAdmissionCleanupError
		if errors.As(node, &retained) && !seen[retained] {
			owners[retained.store] += nodeCounts[retained]
			seen[retained] = true
		}
	}
	for _, store := range stores {
		if owners[store] != 1 {
			t.Fatalf("projection retained Store %p %d times", store, owners[store])
		}
		closeObservationStore(&result, store)
		if errors.Is(result.err, task.ErrLockBusy) {
			t.Fatal("projection attempted to close a Store behind unresolved cleanup")
		}
		if _, err := store.PinRootContext(t.Context()); err != nil {
			t.Fatalf("projection closed Store behind its unresolved lease: %v", err)
		}
	}
	for _, cause := range causes {
		if count := nodeCounts[cause]; count != 1 {
			t.Fatalf("projection retained cause %v %d times", cause, count)
		}
	}
}

func TestContinueDispatchFailureKeepsIdentityAndContinuationMetadata(t *testing.T) {
	for _, test := range []struct {
		record   tasklifecycle.TaskRecordPresence
		presence string
	}{
		{tasklifecycle.RecordAbsent, "not_created"},
		{tasklifecycle.RecordUnknown, "unknown"},
		{tasklifecycle.RecordCreated, "created"},
	} {
		t.Run(test.presence, func(t *testing.T) {
			cause := errors.New("predecessor failure")
			dispatched := tasklifecycle.DispatchResult{
				RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("a", 32), Record: test.record,
				Phase: tasklifecycle.DispatchPredecessor, Failure: admission.WithStage("resolve-predecessor", cause),
			}
			observed := tasklifecycle.ContinuationResult{
				RootID: strings.Repeat("a", 32), PredecessorID: strings.Repeat("b", 32),
				Mode: commonprovider.ContinuationNative, SessionSource: tasklifecycle.ContinuationSessionObserved,
				Dispatch: &dispatched,
			}
			result := continuationCommandResult(observed)
			if result.response.TaskID != dispatched.TaskID || result.response.TaskRecord != test.presence || !errors.Is(result.err, cause) {
				t.Fatalf("dispatched continuation lost successor presence: %+v", result)
			}
			next := result.response.Continuation
			if next == nil || next.PredecessorID != strings.Repeat("b", 32) || next.Mode != string(commonprovider.ContinuationNative) || next.SessionSource != string(tasklifecycle.ContinuationSessionObserved) {
				t.Fatalf("ordinary dispatched continuation lost metadata: %+v", next)
			}
		})
	}
}
