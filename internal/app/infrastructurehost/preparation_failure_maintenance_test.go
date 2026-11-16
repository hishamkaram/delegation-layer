package infrastructurehost

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func maintenancePreparationCause(activity string) *supervisor.MaintenanceError {
	cause := inspection.ErrGroupMigrationBusy
	if activity == "present" {
		cause = pueue.ErrPrivateSupervisorMigrationBusy
	}
	return &supervisor.MaintenanceError{
		Assessment: supervisor.MaintenanceAssessment{Readiness: "blocked", ActiveWork: activity},
		Cause:      cause,
	}
}

// This isolates the actual classification/strict-codec/reconstruction bridge.
// The app's resident transport regression separately proves native serving and
// the later real-store task diagnostic; these cases protect codec consumers.
func TestMaintenancePreparationTransportPreservesQueueReasonAuthority(t *testing.T) {
	for _, fixture := range []struct {
		name, activity, reason string
		fault, projectedFault  error
		independent            bool
	}{
		{"closure_only", "none", "", nil, nil, false},
		{"positive_queue", "present", "active_work", nil, nil, false},
		{"host_identity_fault", "none", "", task.ErrIdentityMismatch, task.ErrIdentityMismatch, false},
		{"host_storage_fault", "none", "", os.ErrPermission, infrastructure.ErrOwnerRequest, true},
		{"host_cleanup_fault", "none", "", &taskdir.LifetimeOwnerCleanupError{}, infrastructure.ErrOwnerRequest, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			diagnostic := maintenancePreparationCause(fixture.activity)
			failure, projected := roundTripMaintenancePreparation(t, errors.Join(diagnostic, fixture.fault))
			if !errors.Is(projected, diagnostic.Cause) || failure.IndependentFault != fixture.independent {
				t.Fatalf("transport lost the exact busy leaf or independent host fault: failure=%+v error=%v", failure, projected)
			}
			if fixture.projectedFault != nil && !errors.Is(projected, fixture.projectedFault) {
				t.Fatal("host fault lost its supported identity during reconstruction")
			}
			if reason := supervisor.FailureReason(projected); reason != fixture.reason {
				t.Errorf("transported reason=%q want=%q", reason, fixture.reason)
			}
		})
	}
}

func TestMaintenancePreparationTransportKeepsClientLocalFaults(t *testing.T) {
	for _, fixture := range []struct {
		name, reason string
		fault        error
	}{
		{"deadline", "observation_failed", context.DeadlineExceeded},
		{"canceled", "observation_failed", context.Canceled},
		{"cleanup", "", &taskdir.LifetimeOwnerCleanupError{}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			_, projected := roundTripMaintenancePreparation(t, maintenancePreparationCause("none"))
			joined := errors.Join(projected, fixture.fault)
			if !errors.Is(joined, inspection.ErrGroupMigrationBusy) || !errors.Is(joined, fixture.fault) || supervisor.MaintenanceBusy(joined) {
				t.Fatalf("client-local fault was lost or normalized into maintenance busy: %v", joined)
			}
			if reason := supervisor.FailureReason(joined); reason != fixture.reason {
				t.Errorf("client-local fault reason=%q want=%q", reason, fixture.reason)
			}
		})
	}
}

func roundTripMaintenancePreparation(t *testing.T, cause error) (infrastructure.PreparationFailure, error) {
	t.Helper()
	failure := (Host{}).ClassifyPreparation(cause)
	outcome := infrastructure.PreparationOutcome{Condition: infrastructure.PreparationFault, Failure: failure}
	data, err := task.MarshalCanonical(outcome)
	if err != nil {
		t.Fatal(err)
	}
	var decoded infrastructure.PreparationOutcome
	if err = task.DecodeStrict(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if err = decoded.Validate(); err != nil {
		t.Fatal(err)
	}
	return decoded.Failure, infrastructurePreparationOutcomeError(decoded)
}
