package inspection

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestAdmissionAttemptsKeepLogicalTaskAndIndependentDeadlines(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	firstID, secondID := strings.Repeat("e", 32), strings.Repeat("f", 32)
	first, err := OpenOperationWithIdentityContext(t.Context(), store, request, binding, firstID, time.Now().Add(-2*AdmissionTimeout))
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, first.Close()) })
	firstPath := filepath.Join(store.Root, "inspections", firstID, requestRecordName)
	before, err := os.ReadFile(firstPath)
	mustInspection(t, err)
	second, err := OpenOperationWithIdentityContext(t.Context(), store, request, binding, secondID, time.Now())
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, second.Close()) })
	if !first.Expired(time.Now()) || second.Expired(time.Now()) {
		t.Fatal("a fresh attempt reused or renewed the old deadline")
	}
	if second.Request().TaskID != request.TaskID || second.Request().Task.TaskID != request.TaskID || second.Request().TaskSHA256 != first.Request().TaskSHA256 {
		t.Fatal("separate inspection identity changed the logical task")
	}
	after, err := os.ReadFile(firstPath)
	mustInspection(t, err)
	if string(before) != string(after) {
		t.Fatal("new attempt rewrote earlier immutable inspection evidence")
	}
	assertInspectionAttemptQueueIdentity(t, second, secondID)
	if _, err = store.OpenTask(request.TaskID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspection attempt created an ordinary task: %v", err)
	}
}

func assertInspectionAttemptQueueIdentity(t *testing.T, operation *Operation, id string) {
	t.Helper()
	identity := operation.Request().SupervisorIdentity()
	if identity.TaskID != id || identity.Label() != operation.Request().Label {
		t.Fatalf("queue identity used logical task rather than attempt: %+v", identity)
	}
	permit, err := operation.ClaimSubmissionContext(t.Context(), time.Now())
	mustInspection(t, err)
	if permit.InspectionSubmissionTaskID() != id {
		t.Fatalf("submission permit used wrong inspection identity: %s", permit.InspectionSubmissionTaskID())
	}
	mustInspection(t, permit.ConsumeInspectionSubmission())
	mustInspection(t, operation.RecordReceiptContext(t.Context(), 7))
	stop, _, err := operation.ClaimStop(operation.Deadline())
	mustInspection(t, err)
	if stop.InspectionStopTaskID() != id {
		t.Fatalf("stop permit used wrong inspection identity: %s", stop.InspectionStopTaskID())
	}
}

func TestAdmissionAttemptLifetimeUsesInternalOperationIdentity(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	id := strings.Repeat("e", 32)
	operation, err := OpenOperationWithIdentityContext(t.Context(), store, request, binding, id, time.Now())
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, operation.Close()) })
	intent, err := operation.RecordLifetimeIntentContext(t.Context(), taskdir.LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "fixture-plan", BootID: "fixture-boot", Deadline: operation.Deadline(),
	})
	mustInspection(t, err)
	if intent.TaskID != id || intent.OwnerSHA256 != operation.Digest() || operation.Request().TaskID != request.TaskID {
		t.Fatalf("lifetime lost its independent inspection identity: %+v", intent)
	}
	loaded, err := LoadOperationContext(t.Context(), store, id)
	mustInspection(t, err)
	mustInspection(t, loaded.Close())
	if _, err = LoadOperationContext(t.Context(), store, request.TaskID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("logical task alias unexpectedly opened the attempt: %v", err)
	}
}
