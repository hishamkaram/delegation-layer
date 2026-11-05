package workspaces

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
	"golang.org/x/sys/unix"
)

// These tests establish resource/checkpoint metadata and lease custody only.
// The integrated Stop caller must separately prove native and queue closure.

func TestRootQuiescenceEmptyAndIdleResources(t *testing.T) {
	t.Run("empty", func(t *testing.T) { requireRootQuiescence(t, newRootQuiescenceStore(t), false) })
	t.Run("not yet admitted", func(t *testing.T) {
		fixture := workspacefixture.NewRecoveryFixture(t)
		requireRootQuiescence(t, fixture.Store, false)
	})
	t.Run("retained", func(t *testing.T) {
		fixture := workspacefixture.NewRecoveryFixture(t)
		recordRootQuiescenceRegistration(t, fixture)
		requireRootQuiescence(t, fixture.Store, false)
	})
	t.Run("failed preparation", func(t *testing.T) {
		fixture := workspacefixture.NewRecoveryFixture(t)
		reservation := reserveRootQuiescenceAdmission(t, fixture.Resource)
		operation := openRootQuiescenceHelper(t, fixture, workspace.OperationProvision, reservation.Subject, "unlaunched", "")
		if err := fixture.Resource.RecordAdmissionAborted(t.Context(), reservation); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.Resource.Registration(t.Context()); !workspace.OperationAbsent(err) {
			t.Fatalf("fixture unexpectedly registered: %v", err)
		}
		if _, err := readOperationProof(t.Context(), operation); err != nil {
			t.Fatal(err)
		}
		requireRootQuiescence(t, fixture.Store, false)
	})
}

func TestRootQuiescenceBindingOnlyAdmissionWithoutReservation(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	owner, err := fixture.Resource.AcquireUseOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	if err = fixture.Resource.BindAdmission(t.Context(), strings.Repeat("d", 32), task.ComputeSHA256([]byte("bound before reservation"))); err != nil {
		t.Fatal(err)
	}
	if err = owner.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.Resource.LatestReservation(t.Context()); !workspace.OperationAbsent(err) {
		t.Fatalf("binding-only fixture acquired a reservation: %v", err)
	}
	ids, err := fixture.Resource.OperationIDs(t.Context())
	if err != nil || len(ids) != 0 {
		t.Fatalf("binding-only fixture authorized a helper: %v, %v", ids, err)
	}
	if err = fixture.Resource.ValidateInventory(t.Context()); err != nil {
		t.Fatal(err)
	}
	// This is a production-store owner-loss state, not native closure or retry
	// authority. The future creator fence must precede openDispatchResource.
	requireRootQuiescence(t, fixture.Store, false)
}

func TestRootQuiescenceActiveUseSurvivesFreeOwnerAndAbsentHelper(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	reservation := reserveRootQuiescenceAdmission(t, fixture.Resource)
	initializeRootQuiescenceOwner(t, fixture.Resource)
	requireRootQuiescence(t, fixture.Store, true)
	if err := fixture.Resource.RecordAdmissionAborted(t.Context(), reservation); err != nil {
		t.Fatal(err)
	}
	requireRootQuiescence(t, fixture.Store, false)
}

func TestRootQuiescenceLaterContentionReleasesEarlierResources(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	initializeRootQuiescenceOwner(t, fixture.Resource)
	request := fixture.Resource.Request()
	request.WorkspaceID, request.TaskID = strings.Repeat("e", 32), strings.Repeat("f", 32)
	second, err := workspace.EstablishResource(t.Context(), fixture.Store, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := second.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	owner, err := second.AcquireUseOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	requireRootQuiescence(t, fixture.Store, true)
	first, _, acquireErr := fixture.Resource.TryAcquireExistingUseOwner(t.Context())
	if first != nil {
		if releaseErr := first.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}
	if acquireErr != nil || first == nil {
		t.Fatalf("later contention leaked earlier resource custody: %v", acquireErr)
	}
}

func TestRootQuiescenceCaptureReceiptStillRequiresReservationClosure(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	reservation, receipt := recordRootQuiescenceReleaseCapture(t, fixture.Resource, false)
	requireRootQuiescence(t, fixture.Store, true)
	if err := fixture.Resource.CloseReservation(t.Context(), reservation, receipt.Status, receipt.OperationID); err != nil {
		t.Fatal(err)
	}
	// Failed capture without discard leaves an idle retained checkout.
	requireRootQuiescence(t, fixture.Store, false)
}

func TestRootQuiescenceClosedReleaseCaptureGap(t *testing.T) {
	t.Run("captured", func(t *testing.T) {
		fixture := workspacefixture.NewRecoveryFixture(t)
		workspacefixture.SeedCanonicalReleaseCapture(t, fixture.Resource)
		requireRootQuiescence(t, fixture.Store, true)
	})
	t.Run("discard unavailable", func(t *testing.T) {
		fixture := workspacefixture.NewRecoveryFixture(t)
		reservation, receipt := recordRootQuiescenceReleaseCapture(t, fixture.Resource, true)
		if err := fixture.Resource.CloseReservation(t.Context(), reservation, receipt.Status, receipt.OperationID); err != nil {
			t.Fatal(err)
		}
		requireRootQuiescence(t, fixture.Store, true)
	})
}

func TestRootQuiescenceClosedNonReleaseWithoutCaptureIDPreservesCanonicalSemantics(t *testing.T) {
	for _, disposition := range []string{"captured", "unavailable"} {
		t.Run(disposition, func(t *testing.T) {
			fixture := workspacefixture.NewRecoveryFixture(t)
			recordRootQuiescenceRegistration(t, fixture)
			closeRootQuiescenceLegacyUse(t, fixture.Resource, disposition)
			requireRootQuiescence(t, fixture.Store, false)
			description, err := Describe(t.Context(), fixture.Resource)
			if err != nil || description.Capture != nil || description.Operation != nil || len(description.Artifacts) != 0 {
				t.Fatalf("closed tag fabricated qualified capture evidence: %+v, %v", description, err)
			}
		})
	}
}

func TestRootQuiescenceRealReleaseWithoutCaptureIDStillFaults(t *testing.T) {
	for _, disposition := range []string{"captured", "unavailable"} {
		t.Run(disposition, func(t *testing.T) {
			fixture := workspacefixture.NewRecoveryFixture(t)
			subject, err := fixture.Resource.RecordReleaseAction(t.Context(), workspace.ReleaseAction{
				SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.Resource.Request().WorkspaceID,
				ID: strings.Repeat("d", 32), BudgetNanos: int64(time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}
			reservation, err := fixture.Resource.Reserve(t.Context(), subject)
			if err != nil {
				t.Fatal(err)
			}
			if err = fixture.Resource.RecordReleaseCapture(t.Context(), reservation, subject); err != nil {
				t.Fatal(err)
			}
			if err = fixture.Resource.CloseReservation(t.Context(), reservation, disposition, ""); err != nil {
				t.Fatal(err)
			}
			before := rootQuiescenceSnapshot(t, fixture.Store.Root)
			guard, acquireErr := AcquireRootQuiescence(t.Context(), fixture.Store)
			closeRootQuiescence(t, guard)
			if !errors.Is(acquireErr, task.ErrEvidenceFault) || PendingError(acquireErr) {
				t.Fatalf("release without qualified receipt lost its fault: %v", acquireErr)
			}
			requireRootQuiescenceUnchanged(t, fixture.Store.Root, before)
		})
	}
}

func TestRootQuiescenceLegacyClosedUseDoesNotHideOtherPendingWork(t *testing.T) {
	for _, pending := range []string{workspace.OperationRemove, workspace.OperationForget, "checkpoint", "owner"} {
		t.Run(pending, func(t *testing.T) {
			fixture := workspacefixture.NewRecoveryFixture(t)
			closeRootQuiescenceLegacyUse(t, fixture.Resource, "unavailable")
			switch pending {
			case "owner":
				owner, err := fixture.Resource.AcquireUseOwner(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if releaseErr := owner.Release(); releaseErr != nil {
						t.Error(releaseErr)
					}
				})
			case "checkpoint":
				operation := newRootQuiescenceCheckpoint(t, fixture.Store)
				if _, err := operation.SealLifetimeUnlaunchedContext(t.Context()); err != nil {
					t.Fatal(err)
				}
			default:
				recordRootQuiescenceAction(t, fixture.Resource, pending)
			}
			requireRootQuiescence(t, fixture.Store, true)
		})
	}
}

func TestRootQuiescenceActionFailuresRequireExplicitRetry(t *testing.T) {
	for _, kind := range []string{workspace.OperationRemove, workspace.OperationForget} {
		for _, state := range []string{"no helper", "active", "unlaunched", "failed", "result unavailable", "succeeded", "completed"} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				fixture := workspacefixture.NewRecoveryFixture(t)
				subject, deadline := recordRootQuiescenceAction(t, fixture.Resource, kind)
				if state != "no helper" {
					openRootQuiescenceHelper(t, fixture, kind, subject, state, deadline)
				}
				if state == "completed" {
					completeRootQuiescenceAction(t, fixture.Resource, kind, subject.ID)
				}
				busy := state == "no helper" || state == "active" || state == "succeeded"
				requireRootQuiescence(t, fixture.Store, busy)
			})
		}
	}
}

func TestRootQuiescencePreservesActionArtifactFault(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	subject, deadline := recordRootQuiescenceAction(t, fixture.Resource, workspace.OperationRemove)
	operation := openRootQuiescenceHelper(t, fixture, workspace.OperationRemove, subject, "result unavailable", deadline)
	result := workspacefixture.SucceededResult(operation)
	result.Status, result.ReasonCode = "failed", "fixture_failed"
	result.Artifacts = []workspace.Artifact{{Name: workspace.FailureDiagnosticName, Length: 1, SHA256: task.ComputeSHA256([]byte("x"))}}
	if err := operation.RecordResult(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	before := rootQuiescenceSnapshot(t, fixture.Store.Root)
	guard, err := AcquireRootQuiescence(t.Context(), fixture.Store)
	closeRootQuiescence(t, guard)
	if !errors.Is(err, os.ErrNotExist) || PendingError(err) {
		t.Fatalf("artifact fault became quiescence or pending: %v", err)
	}
	requireRootQuiescenceUnchanged(t, fixture.Store.Root, before)
}

func TestRootQuiescenceRetainsExistingUseOwnerUntilClose(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	initializeRootQuiescenceOwner(t, fixture.Resource)
	before := rootQuiescenceSnapshot(t, fixture.Store.Root)
	guard, err := AcquireRootQuiescence(t.Context(), fixture.Store)
	t.Cleanup(func() { closeRootQuiescence(t, guard) })
	if err != nil || guard == nil {
		t.Fatalf("idle workspace acquisition = %v, %v", guard, err)
	}
	other, _, acquireErr := fixture.Resource.TryAcquireExistingUseOwner(t.Context())
	if other != nil {
		if releaseErr := other.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}
	if !useOwnerContended(acquireErr) {
		t.Fatalf("closing guard did not retain resource custody: %v", acquireErr)
	}
	closeRootQuiescence(t, guard)
	owner, present, acquireErr := fixture.Resource.TryAcquireExistingUseOwner(t.Context())
	if owner != nil {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}
	if acquireErr != nil || !present || owner == nil {
		t.Fatalf("guard Close retained custody: present %t, %v", present, acquireErr)
	}
	requireRootQuiescenceUnchanged(t, fixture.Store.Root, before)
}

func TestRootQuiescenceBusyOwnerAndJoinedFault(t *testing.T) {
	t.Run("actual contention", func(t *testing.T) {
		fixture := workspacefixture.NewRecoveryFixture(t)
		owner, err := fixture.Resource.AcquireUseOwner(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if releaseErr := owner.Release(); releaseErr != nil {
				t.Error(releaseErr)
			}
		})
		requireRootQuiescence(t, fixture.Store, true)
	})
	t.Run("joined fault", func(t *testing.T) {
		fixture := workspacefixture.NewRecoveryFixture(t)
		initializeRootQuiescenceOwner(t, fixture.Resource)
		injected := errors.New("use owner storage fault")
		fault := &rootQuiescenceLockFault{path: rootQuiescenceOwnerPath(fixture.Resource), operation: unix.LOCK_EX, err: errors.Join(task.ErrLockBusy, injected)}
		fixture.Store.SetFaultInjector(fault)
		t.Cleanup(func() { fixture.Store.SetFaultInjector(nil) })
		guard, err := AcquireRootQuiescence(t.Context(), fixture.Store)
		closeRootQuiescence(t, guard)
		if !fault.fired || !errors.Is(err, injected) || !errors.Is(err, task.ErrLockBusy) || PendingError(err) {
			t.Fatalf("joined owner fault became pure busy: %v", err)
		}
	})
}

func TestRootQuiescenceCleanupFaultReturnsOwnedGuard(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	initializeRootQuiescenceOwner(t, fixture.Resource)
	reserveRootQuiescenceAdmission(t, fixture.Resource)
	injected := errors.New("use owner unlock fault")
	fault := &rootQuiescenceLockFault{path: rootQuiescenceOwnerPath(fixture.Resource), operation: unix.LOCK_UN, err: injected}
	var guard *RootQuiescence
	t.Cleanup(func() {
		fixture.Store.SetFaultInjector(nil)
		closeRootQuiescence(t, guard)
	})
	before := rootQuiescenceSnapshot(t, fixture.Store.Root)
	fixture.Store.SetFaultInjector(fault)
	var err error
	guard, err = AcquireRootQuiescence(t.Context(), fixture.Store)
	if !fault.fired || guard == nil || !errors.Is(err, injected) || !errors.Is(err, workspace.ErrBusy) || PendingError(err) {
		t.Fatalf("cleanup ownership or joined cause lost: guard %v, %v", guard, err)
	}
	fixture.Store.SetFaultInjector(nil)
	closeRootQuiescence(t, guard)
	owner, _, acquireErr := fixture.Resource.TryAcquireExistingUseOwner(t.Context())
	if owner != nil {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	}
	if acquireErr != nil || owner == nil {
		t.Fatalf("retained lease was not releasable: %v", acquireErr)
	}
	requireRootQuiescenceUnchanged(t, fixture.Store.Root, before)
}

func TestRootQuiescenceCancellationDoesNotCreateOrRetainState(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	initializeRootQuiescenceOwner(t, fixture.Resource)
	ctx, cancel := context.WithCancel(t.Context())
	fault := &rootQuiescenceLockFault{path: rootQuiescenceOwnerPath(fixture.Resource), operation: unix.LOCK_EX, cancel: cancel}
	fixture.Store.SetFaultInjector(fault)
	t.Cleanup(func() { fixture.Store.SetFaultInjector(nil) })
	defer cancel()
	before := rootQuiescenceSnapshot(t, fixture.Store.Root)
	guard, err := AcquireRootQuiescence(ctx, fixture.Store)
	closeRootQuiescence(t, guard)
	if !fault.fired || !errors.Is(err, context.Canceled) || PendingError(err) {
		t.Fatalf("cancellation lost at owner boundary: %v", err)
	}
	fixture.Store.SetFaultInjector(nil)
	requireRootQuiescence(t, fixture.Store, false)
	requireRootQuiescenceUnchanged(t, fixture.Store.Root, before)
}

func TestRootQuiescenceInventoryAndRecordFaults(t *testing.T) {
	for _, state := range []string{"orphan directory", "missing directory", "missing request", "request drift", "bad registration", "missing capture receipt", "dangling operation"} {
		t.Run(state, func(t *testing.T) {
			fixture := workspacefixture.NewRecoveryFixture(t)
			corruptRootQuiescenceFixture(t, fixture, state)
			before := rootQuiescenceSnapshot(t, fixture.Store.Root)
			guard, err := AcquireRootQuiescence(t.Context(), fixture.Store)
			closeRootQuiescence(t, guard)
			if err == nil || PendingError(err) {
				t.Fatalf("invalid inventory/evidence accepted: %v", err)
			}
			requireRootQuiescenceUnchanged(t, fixture.Store.Root, before)
		})
	}
}

func TestRootQuiescenceInventoryDiagnosticNamesDifferingIDsWithoutPaths(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	request := fixture.Resource.Request()
	request.WorkspaceID, request.TaskID = strings.Repeat("e", 32), strings.Repeat("a", 32)
	second, err := workspace.EstablishResource(t.Context(), fixture.Store, request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := second.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	moveRootQuiescenceResource(t, fixture.Resource)
	moveRootQuiescenceResource(t, second)
	orphanIDs := []string{strings.Repeat("d", 32), strings.Repeat("f", 32)}
	for _, id := range orphanIDs {
		if mkdirErr := os.Mkdir(filepath.Join(fixture.Store.Root, "workspaces", id), 0o700); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
	}
	before := rootQuiescenceSnapshot(t, fixture.Store.Root)
	guard, acquireErr := AcquireRootQuiescence(t.Context(), fixture.Store)
	closeRootQuiescence(t, guard)
	if !errors.Is(acquireErr, task.ErrEvidenceFault) {
		t.Fatalf("incomplete inventory lost its fault: %v", acquireErr)
	}
	diagnostic := acquireErr.Error()
	for _, id := range append(orphanIDs, fixture.Resource.Request().WorkspaceID, second.Request().WorkspaceID) {
		if !strings.Contains(diagnostic, id) {
			t.Fatalf("inventory diagnostic omitted workspace %s: %v", id, acquireErr)
		}
	}
	if !strings.Contains(diagnostic, "missing directories for committed IDs") || !strings.Contains(diagnostic, "orphan directory IDs") {
		t.Fatalf("inventory diagnostic did not distinguish the mismatch: %v", acquireErr)
	}
	if strings.Contains(diagnostic, string(filepath.Separator)) {
		t.Fatalf("inventory diagnostic disclosed a path: %v", acquireErr)
	}
	requireRootQuiescenceUnchanged(t, fixture.Store.Root, before)
}

func TestRootQuiescenceInventoryDiagnosticBoundsIDLists(t *testing.T) {
	fixture := workspacefixture.NewRecoveryFixture(t)
	orphans := make([]string, 20)
	for index := range orphans {
		orphans[index] = fmt.Sprintf("%032x", index+1)
		if err := os.Mkdir(filepath.Join(fixture.Store.Root, "workspaces", orphans[index]), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	moveRootQuiescenceResource(t, fixture.Resource)
	before := rootQuiescenceSnapshot(t, fixture.Store.Root)
	guard, err := AcquireRootQuiescence(t.Context(), fixture.Store)
	closeRootQuiescence(t, guard)
	if !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("inventory mismatch lost its evidence fault: %v", err)
	}
	diagnostic := err.Error()
	for _, id := range orphans[:16] {
		if !strings.Contains(diagnostic, id) {
			t.Fatalf("diagnostic omitted an ID inside its sample: %s", id)
		}
	}
	for _, id := range orphans[16:] {
		if strings.Contains(diagnostic, id) {
			t.Fatalf("diagnostic exceeded its bounded ID sample: %s", id)
		}
	}
	if !strings.Contains(diagnostic, "20 total") || !strings.Contains(diagnostic, "4 omitted") {
		t.Fatalf("diagnostic omitted its total/omitted counts: %v", err)
	}
	if strings.Contains(diagnostic, string(filepath.Separator)) || len(diagnostic) > 1500 {
		t.Fatal("inventory diagnostic disclosed a path or exceeded its bound")
	}
	requireRootQuiescenceUnchanged(t, fixture.Store.Root, before)
}

func TestRootQuiescenceCheckpointReceiptsAreSettlementAuthority(t *testing.T) {
	store := newRootQuiescenceStore(t)
	original := newRootQuiescenceCheckpoint(t, store)
	if _, err := original.SealLifetimeUnlaunchedContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	requireRootQuiescence(t, store, true)
	if _, err := QualifyCheckpoint(t.Context(), store, original); err != nil {
		t.Fatal(err)
	}
	requireRootQuiescence(t, store, false)
	removal := openCheckpointRemoval(t, store, original, strings.Repeat("2", 32), time.Now().Add(-time.Second))
	registerCheckpointRemovalForTest(t, original, removal, 0)
	if _, err := removal.SealLifetimeUnlaunchedContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	requireRootQuiescence(t, store, true)
	if err := FinalizeCheckpoint(t.Context(), store, removal); err != nil {
		t.Fatal(err)
	}
	requireRootQuiescence(t, store, false)
}

func TestRootQuiescenceCheckpointHistoryRejectsMissingOrForgedPredecessor(t *testing.T) {
	for _, state := range []string{"missing receipt", "forged request"} {
		t.Run(state, func(t *testing.T) {
			store := newRootQuiescenceStore(t)
			original := newRootQuiescenceCheckpoint(t, store)
			if _, err := QualifyCheckpoint(t.Context(), store, original); err != nil {
				t.Fatal(err)
			}
			first := openCheckpointRemoval(t, store, original, strings.Repeat("2", 32), time.Now().Add(-time.Second))
			registerCheckpointRemovalForTest(t, original, first, 0)
			if err := FinalizeCheckpoint(t.Context(), store, first); err != nil {
				t.Fatal(err)
			}
			head := openCheckpointRemoval(t, store, original, strings.Repeat("3", 32), time.Now().Add(-time.Second))
			registerCheckpointRemovalForTest(t, original, head, 1)
			corruptRootQuiescenceCheckpoint(t, store, original, first, state)
			before := rootQuiescenceSnapshot(t, store.Root)
			guard, err := AcquireRootQuiescence(t.Context(), store)
			closeRootQuiescence(t, guard)
			if err == nil || PendingError(err) {
				t.Fatalf("invalid removal history became pure pending: %v", err)
			}
			requireRootQuiescenceUnchanged(t, store.Root, before)
		})
	}
}

func newRootQuiescenceStore(t *testing.T) *taskdir.Store {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return store
}

func newRootQuiescenceCheckpoint(t *testing.T, store *taskdir.Store) *workspace.Operation {
	t.Helper()
	selection := workspace.CheckpointSelection{Kind: workspace.CheckpointOrdinary, Cwd: filepath.Join(t.TempDir(), "checkout"), CwdIdentity: &workspace.DirectoryIdentity{Device: 1, Inode: 2}}
	return openCheckpointCreation(t, store, strings.Repeat("1", 32), "", selection, time.Now().Add(-time.Second))
}

func reserveRootQuiescenceAdmission(t *testing.T, resource *workspace.Resource) workspace.Reservation {
	t.Helper()
	request := resource.Request()
	reservation, err := resource.Reserve(t.Context(), workspace.Subject{Kind: workspace.SubjectAdmission, ID: request.TaskID, SHA256: request.RequestSHA256})
	if err != nil {
		t.Fatal(err)
	}
	return reservation
}

func closeRootQuiescenceLegacyUse(t *testing.T, resource *workspace.Resource, disposition string) {
	t.Helper()
	reservation := reserveRootQuiescenceAdmission(t, resource)
	request := resource.Request()
	subject := workspace.Subject{Kind: workspace.SubjectTask, ID: request.TaskID, SHA256: request.RequestSHA256}
	if err := resource.RecordCaptureObligation(t.Context(), reservation, subject); err != nil {
		t.Fatal(err)
	}
	if err := resource.CloseReservation(t.Context(), reservation, disposition, ""); err != nil {
		t.Fatal(err)
	}
}

func initializeRootQuiescenceOwner(t *testing.T, resource *workspace.Resource) {
	t.Helper()
	owner, err := resource.AcquireUseOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	if err = owner.Release(); err != nil {
		t.Fatal(err)
	}
}

func recordRootQuiescenceRegistration(t *testing.T, fixture *workspacefixture.RecoveryFixture) {
	t.Helper()
	source := workspacefixture.RecoverySource(fixture)
	if err := fixture.Resource.RecordSource(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	checkout, admin := filepath.Join(source.Root, "checkout"), filepath.Join(source.GitDir, "worktrees", "fixture")
	registration := workspace.Registration{
		SchemaVersion: task.SchemaVersion, WorkspaceID: fixture.Resource.Request().WorkspaceID,
		SourceRoot: source.Root, CommonDir: source.CommonDir, GitDir: source.GitDir,
		AdminDir: admin, AdminBacklink: admin, CheckoutPath: checkout, CanonicalCwd: checkout,
		RelativeCwd: ".", BaseOID: source.BaseOID, ObjectFormat: source.ObjectFormat,
		ContainerIdentity: workspace.DirectoryIdentity{Device: 1, Inode: 2}, CheckoutIdentity: workspace.DirectoryIdentity{Device: 3, Inode: 4},
	}
	if err := fixture.Resource.RecordRegistration(t.Context(), registration); err != nil {
		t.Fatal(err)
	}
}

func recordRootQuiescenceReleaseCapture(t *testing.T, resource *workspace.Resource, discard bool) (workspace.Reservation, workspace.CaptureReceipt) {
	t.Helper()
	subject, err := resource.RecordReleaseAction(t.Context(), workspace.ReleaseAction{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, ID: strings.Repeat("d", 32), Discard: discard, BudgetNanos: int64(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := resource.Reserve(t.Context(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if err = resource.RecordReleaseCapture(t.Context(), reservation, subject); err != nil {
		t.Fatal(err)
	}
	receipt := workspace.CaptureReceipt{
		SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID,
		OperationID: strings.Repeat("e", 32), Subject: subject, Status: "unavailable",
		ReasonCode: "capture_not_launched", UnlaunchedSHA256: strings.Repeat("a", 64),
	}
	if err = resource.RecordCaptureReceipt(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	return reservation, receipt
}

func recordRootQuiescenceAction(t *testing.T, resource *workspace.Resource, kind string) (workspace.Subject, string) {
	t.Helper()
	id := strings.Repeat("d", 32)
	if _, err := resource.RecordReleaseAction(t.Context(), workspace.ReleaseAction{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, ID: id, BudgetNanos: int64(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	release := workspace.ReleaseRecord{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: id}
	if err := resource.RecordRelease(t.Context(), release); err != nil {
		t.Fatal(err)
	}
	if kind == workspace.OperationRemove {
		subject, err := RemovalSubject(release)
		if err != nil {
			t.Fatal(err)
		}
		return subject, ""
	}
	if err := resource.RecordRemoved(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	intent := workspace.ForgetIntent{SchemaVersion: task.SchemaVersion, WorkspaceID: resource.Request().WorkspaceID, OperationID: strings.Repeat("f", 32), BudgetNanos: int64(time.Minute), Deadline: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)}
	if err := resource.RecordForgetIntent(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	subject, err := ForgetSubject(intent)
	if err != nil {
		t.Fatal(err)
	}
	return subject, intent.Deadline
}

func openRootQuiescenceHelper(t *testing.T, fixture *workspacefixture.RecoveryFixture, kind string, subject workspace.Subject, state, deadline string) *workspace.Operation {
	t.Helper()
	request := workspacefixture.RecoveryOperationRequest(t, fixture, strings.Repeat("a", 32), kind, subject)
	if deadline != "" {
		request.Deadline = deadline
	} else if state == "unlaunched" {
		request.Deadline = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	}
	operation := workspacefixture.OpenSelectionOperation(t, fixture, request)
	if kind == workspace.OperationForget {
		intent, err := fixture.Resource.ForgetIntent(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = fixture.Resource.AssociateOperation(t.Context(), ForgetPurpose(intent), request); err != nil {
			t.Fatal(err)
		}
	}
	if state == "active" {
		return operation
	}
	if state == "unlaunched" {
		if _, err := operation.SealLifetimeUnlaunchedContext(t.Context()); err != nil {
			t.Fatal(err)
		}
		return operation
	}
	// Production records only: this fixture creates no native scope or actor.
	seedRecoveredCaptureClosure(t, operation, task.LifetimeClosureLegacyPreviousBoot)
	if state == "result unavailable" {
		return operation
	}
	result := workspacefixture.SucceededResult(operation)
	if state == "failed" {
		result.Status, result.ReasonCode = "failed", "fixture_failed"
	}
	if err := operation.RecordResult(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	return operation
}

func completeRootQuiescenceAction(t *testing.T, resource *workspace.Resource, kind, id string) {
	t.Helper()
	var err error
	if kind == workspace.OperationRemove {
		err = resource.RecordRemoved(t.Context(), id)
	} else {
		err = resource.RecordForgotten(t.Context(), id)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func corruptRootQuiescenceFixture(t *testing.T, fixture *workspacefixture.RecoveryFixture, state string) {
	t.Helper()
	directory := filepath.Join(fixture.Store.Root, "workspaces", fixture.Resource.Request().WorkspaceID)
	switch state {
	case "orphan directory":
		if err := os.Mkdir(filepath.Join(filepath.Dir(directory), strings.Repeat("f", 32)), 0o700); err != nil {
			t.Fatal(err)
		}
	case "missing directory":
		if err := fixture.Resource.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(directory, filepath.Join(t.TempDir(), "retained-workspace")); err != nil {
			t.Fatal(err)
		}
	case "request drift":
		request := fixture.Resource.Request()
		request.RequestSHA256 = strings.Repeat("f", 64)
		writeRootQuiescenceRecord(t, filepath.Join(directory, "request.json"), request)
	case "missing request":
		if err := os.Remove(filepath.Join(directory, "request.json")); err != nil {
			t.Fatal(err)
		}
	default:
		corruptRootQuiescenceEvidence(t, fixture, state)
	}
}

func moveRootQuiescenceResource(t *testing.T, resource *workspace.Resource) {
	t.Helper()
	if err := resource.Close(); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(resource.StateRoot(), "workspaces", resource.Request().WorkspaceID)
	if err := os.Rename(directory, filepath.Join(t.TempDir(), "retained-workspace")); err != nil {
		t.Fatal(err)
	}
}

func corruptRootQuiescenceEvidence(t *testing.T, fixture *workspacefixture.RecoveryFixture, state string) {
	t.Helper()
	directory := filepath.Join(fixture.Store.Root, "workspaces", fixture.Resource.Request().WorkspaceID)
	switch state {
	case "bad registration":
		if err := os.WriteFile(filepath.Join(directory, "registration.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "missing capture receipt":
		reservation, receipt := recordRootQuiescenceReleaseCapture(t, fixture.Resource, false)
		if err := fixture.Resource.CloseReservation(t.Context(), reservation, receipt.Status, receipt.OperationID); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(directory, "capture-receipt-"+receipt.OperationID+".json")); err != nil {
			t.Fatal(err)
		}
	case "dangling operation":
		request := workspacefixture.RecoveryOperationRequest(t, fixture, strings.Repeat("a", 32), workspace.OperationProvision, workspace.Subject{Kind: workspace.SubjectAdmission, ID: fixture.Resource.Request().TaskID, SHA256: fixture.Resource.Request().RequestSHA256})
		if err := fixture.Resource.RecordOperation(t.Context(), request); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown corruption fixture %q", state)
	}
}

func corruptRootQuiescenceCheckpoint(t *testing.T, store *taskdir.Store, original, first *workspace.Operation, state string) {
	t.Helper()
	if state == "missing receipt" {
		path := filepath.Join(store.Root, "workspace-operations", original.Request().OperationID, "removal-receipt-00000000000000000001.json")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		return
	}
	request := first.Request()
	request.Subject.SHA256 = strings.Repeat("b", 64)
	writeRootQuiescenceRecord(t, filepath.Join(store.Root, "workspace-operations", request.OperationID, "request.json"), request)
}

func writeRootQuiescenceRecord[T workspace.ResourceRequest | workspace.OperationRequest](t *testing.T, path string, record T) {
	t.Helper()
	data, err := task.MarshalCanonical(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

type rootQuiescenceLockFault struct {
	taskdir.BaseFaultInjector
	path      string
	operation int
	err       error
	cancel    context.CancelFunc
	fired     bool
}

func (*rootQuiescenceLockFault) OnLockFileBarrier(string) error      { return nil }
func (*rootQuiescenceLockFault) OnLockParentDirBarrier(string) error { return nil }
func (f *rootQuiescenceLockFault) OnFlock(path string, operation int) error {
	if f.fired || path != f.path || operation&f.operation == 0 {
		return nil
	}
	f.fired = true
	if f.cancel != nil {
		f.cancel()
	}
	return f.err
}

func rootQuiescenceOwnerPath(resource *workspace.Resource) string {
	return filepath.Join(resource.StateRoot(), "workspaces", resource.Request().WorkspaceID, ".lifetime-owner.lock")
}

func closeRootQuiescence(t *testing.T, guard *RootQuiescence) {
	t.Helper()
	if err := guard.Close(); err != nil {
		t.Error(err)
	}
}

func requireRootQuiescence(t *testing.T, store *taskdir.Store, busy bool) {
	t.Helper()
	before := rootQuiescenceSnapshot(t, store.Root)
	guard, err := AcquireRootQuiescence(t.Context(), store)
	t.Cleanup(func() { closeRootQuiescence(t, guard) })
	if busy {
		if !PendingError(err) {
			t.Fatalf("quiescence = %v, want whole busy/pending", err)
		}
	} else if err != nil || guard == nil {
		t.Fatalf("settled workspace refused: guard %v, %v", guard, err)
	}
	closeRootQuiescence(t, guard)
	requireRootQuiescenceUnchanged(t, store.Root, before)
}

type rootQuiescenceEntry struct {
	mode     os.FileMode
	modified time.Time
	content  string
}

func rootQuiescenceSnapshot(t *testing.T, root string) map[string]rootQuiescenceEntry {
	t.Helper()
	entries := make(map[string]rootQuiescenceEntry)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := rootQuiescenceEntry{mode: info.Mode(), modified: info.ModTime()}
		if info.Mode().IsRegular() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			value.content = string(data)
		}
		entries[path] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func requireRootQuiescenceUnchanged(t *testing.T, root string, before map[string]rootQuiescenceEntry) {
	t.Helper()
	if after := rootQuiescenceSnapshot(t, root); !reflect.DeepEqual(before, after) {
		t.Fatal("read-only quiescence changed durable storage")
	}
}
