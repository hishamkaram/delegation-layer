package app

import (
	"errors"
	"os"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func TestTaskCommandProjectionRetainsAdmissionCleanup(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	lease, err := store.AcquireRootAdmissionContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := lease.Release(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	releaseErr := errors.New("native admission release failed")
	for _, test := range []struct {
		name  string
		build func() commandResult
		store *taskdir.Store
	}{
		{name: "dispatch", store: store, build: func() commandResult {
			return dispatchCommandResult(tasklifecycle.DispatchResult{
				Creation:             &tasklifecycle.CreationResult{Submission: &tasklifecycle.SubmissionResult{}},
				RootAdmissionCleanup: lease, StoreCleanup: store, RootAdmissionReleaseError: releaseErr,
			})
		}},
		{name: "dispatched continuation", store: store, build: func() commandResult {
			return continuationCommandResult(tasklifecycle.ContinuationResult{
				Mode: commonprovider.ContinuationNative,
				Dispatch: &tasklifecycle.DispatchResult{Creation: &tasklifecycle.CreationResult{
					Submission: &tasklifecycle.SubmissionResult{},
				}},
				RootAdmissionCleanup: lease, StoreCleanup: store, RootAdmissionReleaseError: releaseErr,
			})
		}},
		{name: "successful submission", build: func() commandResult {
			return creationCommandResult(newResponse("dispatch"), "fixture:test", tasklifecycle.CreationResult{
				Submission: &tasklifecycle.SubmissionResult{}, RootAdmissionCleanup: lease, RootAdmissionReleaseError: releaseErr,
			})
		}},
		{name: "existing task", build: func() commandResult {
			return existingCommandResult(newResponse("dispatch"), "fixture:test", tasklifecycle.ExistingTaskResult{
				RootAdmissionCleanup: lease, RootAdmissionReleaseError: releaseErr,
			})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := test.build()
			var cleanup *taskAdmissionCleanupError
			if result.code != 1 || !errors.Is(result.err, releaseErr) || !errors.As(result.err, &cleanup) {
				t.Fatalf("cleanup lost in command projection: code=%d err=%v", result.code, result.err)
			}
			if cleanup.lease != lease || cleanup.store != test.store {
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
	lease, err := store.AcquireRootAdmissionContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
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
		cleanupErr = supervisorcore.ReleaseOwnedRootAdmission(lease, nil)
	} else {
		owner := &workspaceCommandResources{store: store, rootAdmission: lease}
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
