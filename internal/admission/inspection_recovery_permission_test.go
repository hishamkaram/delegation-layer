package admission

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInspectionRecoveryRechecksPermissionAfterReceipt(t *testing.T) {
	for _, joinedUnknown := range []bool{false, true} {
		name := "canonical refusal"
		if joinedUnknown {
			name = "refusal joined with queue uncertainty"
		}
		t.Run(name, func(t *testing.T) { assertInspectionRecoveryRechecksPermission(t, joinedUnknown) })
	}
}

func assertInspectionRecoveryRechecksPermission(t *testing.T, joinedUnknown bool) {
	t.Helper()
	store := supervisorfixture.NewInspectionStore(t)
	bound, logPath, _ := admissionfixture.NewUncertainGroupSupervisor(t, store.RootID)
	operation := admissionfixture.NewInspectionPollTestOperationAtWithOwnership(t, store, bound, time.Now().UTC().Add(-2*inspection.AdmissionTimeout), task.RunnerOwnershipManaged)
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	admissionfixture.WriteInspectionPollTestState(t, statusPath, operation, map[string]any{
		"Stashed": map[string]any{"enqueue_at": nil},
	})
	client := newInspectionPollTestClient(t, bound, logPath, nil)
	observation, err := client.ReconcileInspection(t.Context(), operation.Request().SupervisorIdentity())
	if err != nil || !observation.Matched || observation.Job.State != pueue.StateStashed {
		t.Fatalf("stashed setup = %+v, %v", observation, err)
	}
	check := func(ctx context.Context) error {
		permissionErr := operation.CheckLaunchPermission(ctx)
		if permissionErr != nil && joinedUnknown {
			return errors.Join(permissionErr, pueue.ErrUnknown)
		}
		return permissionErr
	}
	commits := 0
	receipt := func(current pueue.InspectionObservation) error {
		commits++
		if receiptErr := operation.RecordReceiptContext(t.Context(), current.Job.ID); receiptErr != nil {
			return receiptErr
		}
		_, fenceErr := operation.SealLifetimeUnlaunchedContext(t.Context())
		return fenceErr
	}
	fallbacks := 0
	match := func(int64) error { fallbacks++; return nil }
	_, err = supervisorcore.RecoverStagedAuxiliaryObservationChecked(t.Context(), client, store.Root, observation, receipt, match, nil, check)
	if !errors.Is(err, task.ErrLifetimeUnlaunched) || commits != 1 || fallbacks != 0 {
		t.Fatalf("closed recovery err=%v receipt commits=%d uncertainty fallbacks=%d", err, commits, fallbacks)
	}
	assertInspectionRecoveryHasNoEnqueue(t, logPath)
	// The receipt remains historical evidence after the failed launch.
	if replayErr := operation.RecordReceiptContext(t.Context(), observation.Job.ID); replayErr != nil {
		t.Fatal(replayErr)
	}
}

func assertInspectionRecoveryHasNoEnqueue(t *testing.T, logPath string) {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, invocation := range strings.Split(string(data), "---\n") {
		if strings.Contains(invocation, "arg=enqueue\n") && !strings.Contains(invocation, "arg=--help\n") {
			t.Fatalf("closed inspection launched enqueue: %s", invocation)
		}
	}
}
