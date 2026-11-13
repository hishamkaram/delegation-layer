package infrastructurehost

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInfrastructurePreparationFailurePreservesExactDomainLeaves(t *testing.T) {
	for _, fixture := range []struct {
		err  error
		want infrastructure.PreparationCauses
	}{
		{pueue.ErrPrivateSupervisorMigrationRequired, infrastructure.PreparationCausePrivateMigrationRequired},
		{pueue.ErrPrivateSupervisorMigrationBusy, infrastructure.PreparationCausePrivateMigrationBusy},
		{pueue.ErrHistoricalTargetUnsupported, infrastructure.PreparationCauseHistoricalTargetUnsupported},
		{inspection.ErrGroupMigrationRequired, infrastructure.PreparationCauseInspectionMigrationRequired},
		{inspection.ErrGroupMigrationBusy, infrastructure.PreparationCauseInspectionMigrationBusy},
		{inspection.ErrGroupMigrationUnsupported, infrastructure.PreparationCauseInspectionMigrationUnsupported},
		{pueue.ErrPrivateSupervisorTransitionIncomplete, infrastructure.PreparationCauseTransitionIncomplete},
		{pueue.ErrPrivateSupervisorNotInitialized, infrastructure.PreparationCauseNotInitialized},
		{pueue.ErrPrivateEndpointRefused, infrastructure.PreparationCauseEndpointRefused},
		{pueue.ErrBinding, infrastructure.PreparationCauseBinding},
		{pueue.ErrConfiguration, infrastructure.PreparationCauseConfiguration},
		{pueue.ErrUnknown, infrastructure.PreparationCauseUnknown},
		{pueue.ErrInFlight, infrastructure.PreparationCauseInFlight},
		{pueue.ErrStatusOperational, infrastructure.PreparationCauseStatusOperational},
		{pueue.ErrControlLimit, infrastructure.PreparationCauseControlLimit},
		{task.ErrLockBusy, infrastructure.PreparationCauseLockBusy},
		{context.DeadlineExceeded, infrastructure.PreparationCauseDeadline},
		{context.Canceled, infrastructure.PreparationCauseCanceled},
		{infrastructure.ErrIdentity, infrastructure.PreparationCauseInfrastructureIdentity},
		{task.ErrIdentityMismatch, infrastructure.PreparationCauseTaskIdentity},
		{task.ErrEvidenceFault, infrastructure.PreparationCauseEvidenceFault},
		{pueue.ErrPrivateRuntimeRestartPending, infrastructure.PreparationCauseRuntimeRestartPending},
		{supervisor.ErrRuntimeQuiescenceBusy, infrastructure.PreparationCauseRuntimeQuiescenceBusy},
	} {
		t.Run(fixture.err.Error(), func(t *testing.T) {
			failure := (Host{}).ClassifyPreparation(fixture.err)
			if failure.Causes != fixture.want || failure.IndependentFault {
				t.Fatalf("exact leaf classification changed: %+v", failure)
			}
			projected := infrastructurePreparationOutcomeError(infrastructure.PreparationOutcome{Condition: infrastructure.PreparationFault, Failure: failure})
			if !errors.Is(projected, fixture.err) || errors.Is(projected, infrastructure.ErrOwnerRequest) {
				t.Fatalf("known domain leaf gained a generic fault: %v", projected)
			}
		})
	}
}

func TestRuntimeRestartPendingPreparationKeepsExactCauseAcrossWire(t *testing.T) {
	assertRuntimeRestartMarkerWire(t)
	for _, fixture := range []struct {
		name  string
		cause error
		bits  infrastructure.PreparationCauses
	}{
		{"deadline", context.DeadlineExceeded, infrastructure.PreparationCauseDeadline},
		{"canceled", context.Canceled, infrastructure.PreparationCauseCanceled},
		{"inflight", pueue.ErrInFlight, infrastructure.PreparationCauseInFlight},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			original := &pueue.PrivateRuntimeRestartError{ReasonCode: "successor_not_ready", Err: errors.Join(pueue.ErrPrivateRuntimeRestartPending, fixture.cause)}
			assertRuntimeRestartObservationWire(t, original, fixture.bits)
		})
	}
	assertRuntimeRestartObservationFaults(t)
}

func assertRuntimeRestartMarkerWire(t *testing.T) {
	t.Helper()
	failure := (Host{}).ClassifyPreparation(pueue.ErrPrivateRuntimeRestartPending)
	if failure.Causes == 0 || failure.IndependentFault {
		t.Fatalf("runtime restart fence became an opaque setup fault: %+v", failure)
	}
	data, err := task.MarshalCanonical(failure)
	if err != nil {
		t.Fatal(err)
	}
	var decoded infrastructure.PreparationFailure
	if err = task.DecodeStrict(data, &decoded); err != nil {
		t.Fatal(err)
	}
	projected := infrastructurePreparationOutcomeError(infrastructure.PreparationOutcome{Condition: infrastructure.PreparationPending, Failure: decoded})
	if !errors.Is(projected, pueue.ErrPrivateRuntimeRestartPending) || !errors.Is(projected, infrastructure.ErrOwnerPending) || errors.Is(projected, infrastructure.ErrOwnerRequest) {
		t.Fatalf("runtime restart pending lost exact retry identity: %v", projected)
	}
	for _, cause := range []error{os.ErrPermission, &retainedPreparationFixtureError{pueue.ErrPrivateRuntimeRestartPending}} {
		mixed := (Host{}).ClassifyPreparation(errors.Join(pueue.ErrPrivateRuntimeRestartPending, cause))
		if mixed.Causes != failure.Causes || !mixed.IndependentFault {
			t.Fatalf("joined storage or cleanup failure became pure pending: %+v", mixed)
		}
		projected = infrastructurePreparationOutcomeError(infrastructure.PreparationOutcome{Condition: infrastructure.PreparationFault, Failure: mixed})
		if !errors.Is(projected, pueue.ErrPrivateRuntimeRestartPending) || !errors.Is(projected, infrastructure.ErrOwnerRequest) {
			t.Fatalf("mixed pending lost its independent fault: %v", projected)
		}
	}
}

func assertRuntimeRestartObservationWire(t *testing.T, original error, observation infrastructure.PreparationCauses) {
	t.Helper()
	failure := (Host{}).ClassifyPreparation(original)
	want := infrastructure.PreparationFailure{Causes: infrastructure.PreparationCauseRuntimeRestartPending | observation, Reason: infrastructure.PreparationReasonObservationFailed}
	if failure != want {
		t.Fatalf("typed known-born observation lost exact causes/reason or gained Binding/fault: got=%+v want=%+v", failure, want)
	}
	decoded := roundTripRuntimeRestartFailure(t, failure)
	// This isolates the app's classification/reconstruction bridge. The
	// server's actual timeout selection has its own component/native coverage.
	projected := infrastructurePreparationOutcomeError(infrastructure.PreparationOutcome{Condition: infrastructure.PreparationPending, Failure: decoded})
	if !errors.Is(projected, infrastructure.ErrOwnerPending) || !errors.Is(projected, pueue.ErrPrivateRuntimeRestartPending) || errors.Is(projected, pueue.ErrBinding) || errors.Is(projected, infrastructure.ErrOwnerRequest) || supervisor.FailureReason(projected) != "observation_failed" {
		t.Fatalf("typed known-born observation became an independent setup fault: %v", projected)
	}
}

func assertRuntimeRestartObservationFaults(t *testing.T) {
	t.Helper()
	for _, fixture := range []struct {
		name        string
		extra       error
		bits        infrastructure.PreparationCauses
		independent bool
	}{
		{"binding", pueue.ErrBinding, infrastructure.PreparationCauseBinding, false},
		{"retained cleanup", &retainedPreparationFixtureError{cause: pueue.ErrPrivateRuntimeRestartPending}, 0, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			original := &pueue.PrivateRuntimeRestartError{ReasonCode: "successor_not_ready", Err: errors.Join(pueue.ErrPrivateRuntimeRestartPending, context.DeadlineExceeded, fixture.extra)}
			failure := (Host{}).ClassifyPreparation(original)
			want := infrastructure.PreparationFailure{Causes: infrastructure.PreparationCauseRuntimeRestartPending | infrastructure.PreparationCauseDeadline | fixture.bits, Reason: infrastructure.PreparationReasonObservationFailed, IndependentFault: fixture.independent}
			if failure != want {
				t.Fatalf("typed observation concealed a binding/cleanup fault: got=%+v want=%+v", failure, want)
			}
			projected := infrastructurePreparationOutcomeError(infrastructure.PreparationOutcome{Condition: infrastructure.PreparationFault, Failure: roundTripRuntimeRestartFailure(t, failure)})
			if errors.Is(projected, infrastructure.ErrOwnerPending) || !errors.Is(projected, pueue.ErrPrivateRuntimeRestartPending) || errors.Is(projected, infrastructure.ErrOwnerRequest) != fixture.independent || errors.Is(projected, pueue.ErrBinding) != (fixture.bits != 0) {
				t.Fatalf("mixed typed failure lost its fault identity: %v", projected)
			}
		})
	}
}

func roundTripRuntimeRestartFailure(t *testing.T, failure infrastructure.PreparationFailure) infrastructure.PreparationFailure {
	t.Helper()
	data, err := task.MarshalCanonical(failure)
	if err != nil {
		t.Fatal(err)
	}
	var decoded infrastructure.PreparationFailure
	if err = task.DecodeStrict(data, &decoded); err != nil || decoded != failure {
		t.Fatalf("typed runtime failure changed across strict wire: got=%+v want=%+v err=%v", decoded, failure, err)
	}
	return decoded
}

func TestInfrastructurePreparationFailurePreservesJoinedPolicies(t *testing.T) {
	for _, fixture := range []struct {
		name        string
		err         error
		want        infrastructure.PreparationCauses
		independent bool
		deferable   bool
	}{
		{"required binding", errors.Join(pueue.ErrPrivateSupervisorMigrationRequired, pueue.ErrBinding), infrastructure.PreparationCausePrivateMigrationRequired | infrastructure.PreparationCauseBinding, false, false},
		{"unknown inflight", errors.Join(pueue.ErrUnknown, pueue.ErrInFlight), infrastructure.PreparationCauseUnknown | infrastructure.PreparationCauseInFlight, false, false},
		{"pure transition", pueue.ErrPrivateSupervisorTransitionIncomplete, infrastructure.PreparationCauseTransitionIncomplete, false, true},
		{"transition required", errors.Join(pueue.ErrPrivateSupervisorTransitionIncomplete, pueue.ErrPrivateSupervisorMigrationRequired), infrastructure.PreparationCauseTransitionIncomplete | infrastructure.PreparationCausePrivateMigrationRequired, false, false},
		{"transition IO", errors.Join(pueue.ErrPrivateSupervisorTransitionIncomplete, os.ErrPermission), infrastructure.PreparationCauseTransitionIncomplete, true, false},
		{"transition retained", &retainedPreparationFixtureError{pueue.ErrPrivateSupervisorTransitionIncomplete}, infrastructure.PreparationCauseTransitionIncomplete, true, false},
		{"busy retained", &retainedPreparationFixtureError{pueue.ErrPrivateSupervisorMigrationBusy}, infrastructure.PreparationCausePrivateMigrationBusy, true, false},
		{"submission Is alias", pueue.ErrPrivateSupervisorSubmissionIncomplete, 0, true, false},
		{"opaque non-comparable", opaquePreparationFixtureError{"private fixture content"}, 0, true, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			failure := (Host{}).ClassifyPreparation(fixture.err)
			if failure.Causes != fixture.want || failure.IndependentFault != fixture.independent {
				t.Fatalf("whole error classification lost a cause: %+v", failure)
			}
			projected := infrastructurePreparationOutcomeError(infrastructure.PreparationOutcome{Condition: infrastructure.PreparationFault, Failure: failure})
			if supervisor.TaskMaintenanceCompletionRefused(projected) != fixture.deferable {
				t.Fatalf("routing changed canonical deferral policy: %v", projected)
			}
			if errors.Is(projected, infrastructure.ErrOwnerRequest) != fixture.independent {
				t.Fatalf("routing lost or invented an independent fault: %v", projected)
			}
		})
	}
}

type retainedPreparationFixtureError struct{ cause error }

func (*retainedPreparationFixtureError) Error() string           { return "fixture retained preparation" }
func (e *retainedPreparationFixtureError) Unwrap() error         { return e.cause }
func (*retainedPreparationFixtureError) RetainsResidentCleanup() {}

func TestInfrastructurePreparationFailureReasonCannotHideOpaqueFault(t *testing.T) {
	private := "/private/fixture/never-send-to-owner-wire"
	original := &pueue.PrivateRecoveryError{ReasonCode: "state_unreadable", Err: errors.Join(pueue.ErrPrivateSupervisorMigrationRequired, errors.New(private))}
	failure := (Host{}).ClassifyPreparation(original)
	if failure.Reason != infrastructure.PreparationReasonStateUnreadable || failure.Causes != infrastructure.PreparationCausePrivateMigrationRequired || !failure.IndependentFault {
		t.Fatalf("closed reason concealed IO fault: %+v", failure)
	}
	projected := infrastructurePreparationOutcomeError(infrastructure.PreparationOutcome{Condition: infrastructure.PreparationFault, Failure: failure})
	if supervisor.FailureReason(projected) != "state_unreadable" || !errors.Is(projected, pueue.ErrPrivateSupervisorMigrationRequired) || !errors.Is(projected, infrastructure.ErrOwnerRequest) {
		t.Fatalf("reason or failure identities lost: %v", projected)
	}
	if strings.Contains(projected.Error(), private) {
		t.Fatal("projection exposed opaque host diagnostic")
	}
}

func TestInfrastructurePreparationHistoricalRefusalSurvivesWire(t *testing.T) {
	private := "/private/fixture/historical-close-fault"
	for _, opaque := range []bool{false, true} {
		t.Run(map[bool]string{false: "historical refusal", true: "joined opaque fault"}[opaque], func(t *testing.T) {
			cause := errors.Join(pueue.ErrPrivateSupervisorMigrationRequired, pueue.ErrHistoricalTargetNotReady)
			if opaque {
				cause = errors.Join(cause, errors.New(private))
			}
			assertHistoricalPreparationWireRoundTrip(t, cause, private)
		})
	}
}

func assertHistoricalPreparationWireRoundTrip(t *testing.T, cause error, private string) {
	t.Helper()
	original := &pueue.PrivateRecoveryError{ReasonCode: "historical_target_not_ready", Err: cause}
	failure := (Host{}).ClassifyPreparation(original)
	if failure.Causes != infrastructure.PreparationCausePrivateMigrationRequired || failure.Reason != infrastructure.PreparationReasonHistoricalTargetNotReady || !failure.IndependentFault {
		t.Fatalf("historical refusal lost its required cause, reason or unmapped fault: %+v", failure)
	}
	data, err := task.MarshalCanonical(failure)
	if err != nil {
		t.Fatal(err)
	}
	var decoded infrastructure.PreparationFailure
	if err = task.DecodeStrict(data, &decoded); err != nil || decoded != failure {
		t.Fatalf("historical failure wire round trip: %+v: %v", decoded, err)
	}
	projected := infrastructurePreparationOutcomeError(infrastructure.PreparationOutcome{Condition: infrastructure.PreparationFault, Failure: decoded})
	if !errors.Is(projected, pueue.ErrPrivateSupervisorMigrationRequired) || !errors.Is(projected, infrastructure.ErrOwnerRequest) || supervisor.FailureReason(projected) != "historical_target_not_ready" {
		t.Fatalf("historical refusal lost required or independent fault: %v", projected)
	}
	if strings.Contains(string(data), private) || strings.Contains(projected.Error(), private) {
		t.Fatal("historical projection exposed an opaque host diagnostic")
	}
}

func TestInfrastructurePreparationOutcomeKeepsConditionSeparate(t *testing.T) {
	for _, fixture := range []struct {
		condition infrastructure.PreparationCondition
		want      error
	}{
		{infrastructure.PreparationOK, nil},
		{infrastructure.PreparationPending, infrastructure.ErrOwnerPending},
		{infrastructure.PreparationBusy, infrastructure.ErrOwnerBusy},
		{infrastructure.PreparationIncompatible, infrastructure.ErrOwnerIncompatible},
		{infrastructure.PreparationRefused, infrastructure.ErrOwnerRequest},
		{infrastructure.PreparationFault, infrastructure.ErrOwnerRequest},
		{infrastructure.PreparationTimeout, context.DeadlineExceeded},
	} {
		projected := infrastructurePreparationOutcomeError(infrastructure.PreparationOutcome{Condition: fixture.condition})
		if !errors.Is(projected, fixture.want) {
			t.Fatalf("condition %s: %v", fixture.condition, projected)
		}
	}
	if projected := infrastructurePreparationOutcomeError(infrastructure.PreparationOutcome{}); projected != nil {
		t.Fatalf("no verified reply invented a domain error: %v", projected)
	}
	busy := infrastructure.PreparationOutcome{Condition: infrastructure.PreparationBusy, Failure: infrastructure.PreparationFailure{Causes: infrastructure.PreparationCauseLockBusy}}
	if projected := infrastructurePreparationOutcomeError(busy); !errors.Is(projected, task.ErrLockBusy) || errors.Is(projected, infrastructure.ErrOwnerRequest) {
		t.Fatalf("known busy became independent fault: %v", projected)
	}
	invalid := infrastructure.PreparationOutcome{Condition: infrastructure.PreparationFault, Failure: infrastructure.PreparationFailure{Causes: 1 << 31}}
	if projected := infrastructurePreparationOutcomeError(invalid); !errors.Is(projected, infrastructure.ErrIdentity) {
		t.Fatalf("unknown causes were accepted: %v", projected)
	}
}

type opaquePreparationFixtureError []string

func (opaquePreparationFixtureError) Error() string { return "opaque fixture failure" }
