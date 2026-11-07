package workspaces

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/workspace"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

var (
	admissionHelperID   = strings.Repeat("6", 32)
	errDispatchRejected = errors.New("dispatch rejected after provisioning")
)

// finishedAdmission is an admission as PrepareDispatch leaves it: its own
// resource handle and use owner, and a reservation for its dispatch subject.
type finishedAdmission struct {
	fixture     *workspacefixture.RecoveryFixture
	admission   *Admission
	reservation workspace.Reservation
}

func newFinishedAdmission(t *testing.T) finishedAdmission {
	t.Helper()
	fixture := workspacefixture.NewRecoveryFixture(t)
	admission := acquireAdmissionUse(t, fixture)
	request := fixture.Resource.Request()
	reservation, err := admission.resource.Reserve(t.Context(), workspace.Subject{Kind: workspace.SubjectAdmission, ID: request.TaskID, SHA256: request.RequestSHA256})
	if err != nil {
		t.Fatal(err)
	}
	admission.reservation = reservation
	return finishedAdmission{fixture: fixture, admission: admission, reservation: reservation}
}

// acquireAdmissionUse opens the admission's own resource handle and use owner.
func acquireAdmissionUse(t *testing.T, fixture *workspacefixture.RecoveryFixture) *Admission {
	t.Helper()
	resource, err := workspace.LoadResource(t.Context(), fixture.Store, fixture.Resource.Request().WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	admission := &Admission{resource: resource}
	t.Cleanup(func() {
		if cleanupErr := admission.Close(); cleanupErr != nil {
			fixture.RetainCleanupFailure(cleanupErr)
			t.Error(cleanupErr)
		}
	})
	admission.owner, err = resource.AcquireUseOwner(t.Context())
	if err != nil {
		t.Fatal(errors.Join(err, admission.closeAfter(fixture.Store, err)))
	}
	return admission
}

// persistAdmissionReservation reserves the admission subject under a
// transient use owner, for a child process to resume.
func persistAdmissionReservation(t *testing.T, fixture *workspacefixture.RecoveryFixture) workspace.Reservation {
	t.Helper()
	owner, err := fixture.Resource.AcquireUseOwner(t.Context())
	t.Cleanup(func() {
		if cleanupErr := owner.Release(); cleanupErr != nil {
			fixture.RetainCleanupFailure(cleanupErr)
			t.Error(cleanupErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	request := fixture.Resource.Request()
	reservation, err := fixture.Resource.Reserve(t.Context(), workspace.Subject{Kind: workspace.SubjectAdmission, ID: request.TaskID, SHA256: request.RequestSHA256})
	if err = errors.Join(err, owner.Release()); err != nil {
		t.Fatal(err)
	}
	return reservation
}

// resumeFinishedAdmission is newFinishedAdmission over the reservation a
// parent process persisted in fixture.
func resumeFinishedAdmission(t *testing.T, fixture *workspacefixture.RecoveryFixture) finishedAdmission {
	t.Helper()
	admission := acquireAdmissionUse(t, fixture)
	reservation, err := admission.resource.LatestReservation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	admission.reservation = reservation
	return finishedAdmission{fixture: fixture, admission: admission, reservation: reservation}
}

// persistAdmissionWithProvisionHelper persists an admission whose unexpired
// provision helper is bound to its subject.
func persistAdmissionWithProvisionHelper(t *testing.T, fixture *workspacefixture.RecoveryFixture) {
	t.Helper()
	reservation := persistAdmissionReservation(t, fixture)
	closeOperation(t, openWorkspaceHelper(t, fixture, admissionHelperID, workspace.OperationProvision, reservation.Subject, time.Now().Add(time.Hour)))
}

// persistAdmissionWithVerification persists an admission and an unexpired
// verification helper that its description selects.
func persistAdmissionWithVerification(t *testing.T, fixture *workspacefixture.RecoveryFixture) {
	t.Helper()
	persistAdmissionReservation(t, fixture)
	persistUnfinishedVerification(t, fixture)
}

// openProvisionHelper persists a helper bound to the admission subject, as
// provisioning may before dispatch fails.
func (f finishedAdmission) openProvisionHelper(t *testing.T, deadline time.Duration) *workspace.Operation {
	t.Helper()
	return openWorkspaceHelper(t, f.fixture, admissionHelperID, workspace.OperationProvision, f.reservation.Subject, time.Now().Add(deadline))
}

// requireFinalClose asserts Finish released the use owner and its handle.
func (f finishedAdmission) requireFinalClose(t *testing.T, finished AdmissionFinish) {
	t.Helper()
	if finished.CloseError != nil {
		t.Fatalf("final close = %v", finished.CloseError)
	}
	owner, err := f.fixture.Resource.TryAcquireUseOwner(t.Context())
	if err != nil {
		t.Fatalf("finish kept the use owner: %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := owner.Release(); cleanupErr != nil {
			f.fixture.RetainCleanupFailure(cleanupErr)
			t.Error(cleanupErr)
		}
	})
	if err = owner.Release(); err != nil {
		t.Fatal(err)
	}
}

func (f finishedAdmission) requireNotAborted(t *testing.T) {
	t.Helper()
	if _, err := f.fixture.Resource.ReservationClosed(t.Context(), f.reservation.Sequence); !workspace.OperationAbsent(err) {
		t.Fatalf("admission reservation closed: %v", err)
	}
}

func (f finishedAdmission) requireAborted(t *testing.T) {
	t.Helper()
	closed, err := f.fixture.Resource.ReservationClosed(t.Context(), f.reservation.Sequence)
	if err != nil || closed.Disposition != "aborted-not-launched" {
		t.Fatalf("fully closed admission = %+v, %v", closed, err)
	}
}

func TestAdmissionFinishAbortKeepsFaultJoinedWithPending(t *testing.T) {
	fixture := workspacefixture.OwnerReleaseRecoveryFixture(t, persistAdmissionWithProvisionHelper, admissionHelperID)
	if fixture == nil {
		return
	}
	admission := resumeFinishedAdmission(t, fixture)
	fault := injectOwnerUnlockFault(t, fixture.Store, admissionHelperID)
	finished := admission.admission.Finish(fixture.Store, errDispatchRejected, false, nil)
	requirePendingJoinedFault(t, "abort recovery", finished.AbortError, fault)
	admission.requireNotAborted(t)
	admission.requireRetainedFinishThenExplicitClose(t, finished, fault)
}

func TestAdmissionFinishAbortControls(t *testing.T) {
	cases := map[string]struct {
		helper      bool
		taskCreated bool
		aborted     bool
	}{
		"pending only":     {true, false, false},
		"fully closed":     {false, false, true},
		"task was created": {true, true, false},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			admission := newFinishedAdmission(t)
			var fault *ownerUnlockFault
			if test.helper {
				admission.openProvisionHelper(t, time.Hour)
			}
			if test.taskCreated {
				fault = injectOwnerUnlockFault(t, admission.fixture.Store, admissionHelperID)
			}
			finished := admission.admission.Finish(admission.fixture.Store, errDispatchRejected, test.taskCreated, nil)
			if finished.AbortError != nil || (fault != nil && fault.fired) {
				t.Fatalf("abort recovery = %v; want none and no recovery of a created task's helper", finished.AbortError)
			}
			if test.aborted {
				admission.requireAborted(t)
			} else {
				admission.requireNotAborted(t)
			}
			admission.requireFinalClose(t, finished)
		})
	}
}

func TestAdmissionFinishDescriptionKeepsFaultJoinedWithPending(t *testing.T) {
	fixture := workspacefixture.OwnerReleaseRecoveryFixture(t, persistAdmissionWithVerification, genericDescribedHelperID)
	if fixture == nil {
		return
	}
	admission := resumeFinishedAdmission(t, fixture)
	admission.admission.operationID = genericDescribedHelperID
	fault := injectOwnerUnlockFault(t, fixture.Store, genericDescribedHelperID)
	finished := admission.admission.Finish(fixture.Store, nil, true, nil)
	if finished.OperationID != genericDescribedHelperID || finished.AbortError != nil {
		t.Fatalf("finished admission = %+v", finished)
	}
	requirePendingJoinedFault(t, "description error", finished.DescriptionError, fault)
	admission.requireRetainedFinishThenExplicitClose(t, finished, fault)
}

func TestAdmissionFinishDescriptionPendingOnly(t *testing.T) {
	admission := newFinishedAdmission(t)
	openWorkspaceHelper(t, admission.fixture, genericDescribedHelperID, workspace.OperationVerify, verificationSubject(genericDescribedHelperID), time.Now().Add(time.Hour))
	admission.admission.operationID = genericDescribedHelperID
	finished := admission.admission.Finish(admission.fixture.Store, nil, true, nil)
	if finished.OperationID != genericDescribedHelperID || finished.AbortError != nil {
		t.Fatalf("finished admission = %+v", finished)
	}
	if finished.DescriptionError != nil || finished.Description.NextAction != "watch_workspace" {
		t.Fatalf("pending description = %+v, %v", finished.Description, finished.DescriptionError)
	}
	admission.requireFinalClose(t, finished)
}

func (f finishedAdmission) requireRetainedFinishThenExplicitClose(t *testing.T, finished AdmissionFinish, fault *ownerUnlockFault) {
	t.Helper()
	var retained *AdmissionCleanupError
	if !errors.As(finished.CloseError, &retained) || retained.Admission() != f.admission || !errors.Is(finished.CloseError, errOwnerUnlock) {
		t.Fatalf("Finish lost the failed child/admission cleanup owner: %v", finished.CloseError)
	}
	if !f.admission.CleanupPending() || fault.unlocks != 1 {
		t.Fatalf("failed child was not retained without retry: pending=%t unlocks=%d", f.admission.CleanupPending(), fault.unlocks)
	}
	owner, err := f.fixture.Resource.TryAcquireUseOwner(t.Context())
	if owner != nil {
		t.Cleanup(func() {
			if releaseErr := owner.Release(); releaseErr != nil {
				f.fixture.RetainCleanupFailure(releaseErr)
				t.Error(releaseErr)
			}
		})
	}
	if owner != nil || !useOwnerContended(err) {
		t.Fatalf("parent use owner released before failed child: %v", err)
	}
	// Explicit test cleanup follows the failure observation; Finish itself
	// must neither retry this fault nor close the parent behind the child.
	f.fixture.Store.SetFaultInjector(nil)
	closeErr := retained.Admission().Close()
	f.requireFinalClose(t, AdmissionFinish{CloseError: closeErr})
	if retained.Admission().CleanupPending() {
		t.Fatal("explicit child-first cleanup left admission ownership")
	}
}
