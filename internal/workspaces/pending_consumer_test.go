package workspaces

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

// These consumers once continued on any error carrying ErrPending. Each case
// enters the production recovery of an unfinished helper without lifetime
// intent through the production store, whose first owner release fails.

func openUnfinishedHelper(t *testing.T, kind string) (*workspacefixture.RecoveryFixture, *workspace.Operation) {
	t.Helper()
	fixture := workspacefixture.NewRecoveryFixture(t)
	operation := openWorkspaceHelper(t, fixture, genericDescribedHelperID, kind, verificationSubject(genericDescribedHelperID), time.Now().Add(time.Hour))
	return fixture, operation
}

// loadOperation reopens a persisted operation; test cleanup closes it.
func loadOperation(t *testing.T, fixture *workspacefixture.RecoveryFixture, operationID string) *workspace.Operation {
	t.Helper()
	operation, err := workspace.LoadOperation(t.Context(), fixture.Store, operationID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return operation
}

func TestSubmitOperationKeepsReleaseFaultBeforeSupervisor(t *testing.T) {
	fixture := workspacefixture.OwnerReleaseRecoveryFixture(t, persistUnfinishedVerification, genericDescribedHelperID)
	if fixture == nil {
		return
	}
	operation := loadOperation(t, fixture, genericDescribedHelperID)
	request := operation.Request()
	fault := injectOwnerUnlockFault(t, fixture.Store, genericDescribedHelperID)
	err := SubmitOperation(t.Context(), fixture.Store, operation, unavailableRuntimeOptions)
	requirePendingJoinedFault(t, "submission", err, fault)
	if errors.Is(err, errOperationFixtureRuntimeUnavailable) {
		t.Fatalf("submission opened the supervisor after a release fault: %v", err)
	}
	stored, err := workspace.LoadOperation(t.Context(), fixture.Store, genericDescribedHelperID)
	if err != nil {
		t.Fatal(err)
	}
	if storedErr := stored.Close(); storedErr != nil || stored.Request().Deadline != request.Deadline || stored.Digest() != operation.Digest() {
		t.Fatalf("stored request changed: %+v, %v", stored.Request(), storedErr)
	}
}

func TestSubmitOperationControls(t *testing.T) {
	t.Run("pending only reaches the runtime", func(t *testing.T) {
		fixture, operation := openUnfinishedHelper(t, workspace.OperationVerify)
		if err := SubmitOperation(t.Context(), fixture.Store, operation, unavailableRuntimeOptions); !errors.Is(err, errOperationFixtureRuntimeUnavailable) {
			t.Fatalf("pending submission = %v, want the canonical runtime path", err)
		}
	})
	t.Run("closed", func(t *testing.T) {
		fixture, operation := openUnfinishedHelper(t, workspace.OperationVerify)
		workspacefixture.RunRealGuardianLifetime(t, fixture.Store, operation, func() {})
		if err := SubmitOperation(t.Context(), fixture.Store, operation, unavailableRuntimeOptions); err != nil {
			t.Fatalf("closed submission = %v, want nil", err)
		}
	})
}

func TestAwaitProvisionReturnsReleaseFaultWithoutPolling(t *testing.T) {
	fixture := workspacefixture.OwnerReleaseRecoveryFixture(t, persistUnfinishedProvision, genericDescribedHelperID)
	if fixture == nil {
		return
	}
	fault := injectOwnerUnlockFault(t, fixture.Store, genericDescribedHelperID)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := awaitProvision(ctx, fixture.Store, fixture.Resource, genericDescribedHelperID)
	requirePendingJoinedFault(t, "provision wait", err, fault)
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("provision wait polled past the fault: %v", err)
	}
	if _, err = fixture.Resource.Registration(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("faulted provision changed registration: %v", err)
	}
}

// The bubble's clock advances only while every goroutine is durably blocked,
// so record I/O cannot consume the deadline; it expires in the observation
// wait, strictly between two polls.
func TestAwaitProvisionPendingOnlyWaitsForItsDeadline(t *testing.T) {
	const deadline = 250 * time.Millisecond
	synctest.Test(t, func(t *testing.T) {
		fixture, _ := openUnfinishedHelper(t, workspace.OperationProvision)
		ctx, cancel := context.WithTimeout(t.Context(), deadline)
		defer cancel()
		started := time.Now()
		_, err := awaitProvision(ctx, fixture.Store, fixture.Resource, genericDescribedHelperID)
		if !errors.Is(err, workspace.ErrPending) || !errors.Is(err, context.DeadlineExceeded) || !PendingError(err) {
			t.Fatalf("pending provision = %v, want pending at its deadline", err)
		}
		if elapsed := time.Since(started); elapsed != deadline {
			t.Fatalf("pending provision returned after %v, want exactly its %v deadline", elapsed, deadline)
		}
	})
}

func TestForgetIntentKeepsReleaseFaultInsteadOfBusy(t *testing.T) {
	fixture := workspacefixture.OwnerReleaseRecoveryFixture(t, persistUnfinishedVerification, genericDescribedHelperID)
	if fixture == nil {
		return
	}
	fault := injectOwnerUnlockFault(t, fixture.Store, genericDescribedHelperID)
	_, err := EnsureForgetIntent(t.Context(), fixture.Store, fixture.Resource, time.Minute)
	requirePendingJoinedFault(t, "forget inventory", err, fault)
	if errors.Is(err, workspace.ErrBusy) {
		t.Fatalf("release fault became contention: %v", err)
	}
	if _, err = fixture.Resource.ForgetIntent(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("faulted inventory published forget intent: %v", err)
	}
}

func TestForgetIntentControls(t *testing.T) {
	t.Run("pending only is busy", func(t *testing.T) {
		fixture, _ := openUnfinishedHelper(t, workspace.OperationVerify)
		if _, err := EnsureForgetIntent(t.Context(), fixture.Store, fixture.Resource, time.Minute); !errors.Is(err, workspace.ErrBusy) || errors.Is(err, workspace.ErrPending) {
			t.Fatalf("pending inventory = %v, want bare busy", err)
		}
		if _, err := fixture.Resource.ForgetIntent(t.Context()); !workspace.OperationAbsent(err) {
			t.Fatalf("busy inventory published forget intent: %v", err)
		}
	})
	t.Run("closed helper without result", func(t *testing.T) {
		fixture := workspacefixture.NewRecoveryFixture(t)
		openWorkspaceHelper(t, fixture, genericDescribedHelperID, workspace.OperationVerify, verificationSubject(genericDescribedHelperID), time.Now().Add(-time.Second))
		intent, err := ForgetInventory(t.Context(), fixture.Store, fixture.Resource, time.Minute)
		if err != nil || len(intent.Artifacts) != 0 {
			t.Fatalf("fenced helper inventory = %+v, %v", intent, err)
		}
	})
}
