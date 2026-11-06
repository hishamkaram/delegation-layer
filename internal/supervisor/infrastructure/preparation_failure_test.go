package infrastructure

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestHistoricalTargetCauseKeepsRecordedBit(t *testing.T) {
	if PreparationCauseHistoricalTargetUnsupported != 1<<2 {
		t.Fatal("historical target classification changed its recorded bit")
	}
}

func preparationCauseFixtures() []PreparationCauses {
	return []PreparationCauses{
		PreparationCausePrivateMigrationRequired, PreparationCausePrivateMigrationBusy,
		PreparationCauseHistoricalTargetUnsupported, PreparationCauseInspectionMigrationRequired,
		PreparationCauseInspectionMigrationBusy, PreparationCauseInspectionMigrationUnsupported,
		PreparationCauseTransitionIncomplete, PreparationCauseNotInitialized,
		PreparationCauseEndpointRefused, PreparationCauseBinding, PreparationCauseConfiguration,
		PreparationCauseUnknown, PreparationCauseInFlight, PreparationCauseStatusOperational,
		PreparationCauseControlLimit, PreparationCauseLockBusy, PreparationCauseDeadline,
		PreparationCauseCanceled, PreparationCauseInfrastructureIdentity, PreparationCauseTaskIdentity,
		PreparationCauseEvidenceFault,
	}
}

func TestPreparationCausesClosedMask(t *testing.T) {
	var all PreparationCauses
	for _, cause := range preparationCauseFixtures() {
		if cause == 0 || cause&(cause-1) != 0 || all&cause != 0 {
			t.Fatalf("cause is not a unique single bit: %d", cause)
		}
		if err := cause.Validate(); err != nil {
			t.Fatalf("known cause refused: %d: %v", cause, err)
		}
		all |= cause
	}
	for _, causes := range []PreparationCauses{0, all, PreparationCauseTransitionIncomplete | PreparationCausePrivateMigrationBusy} {
		if err := causes.Validate(); err != nil {
			t.Fatalf("valid set refused: %v", err)
		}
	}
	for _, causes := range []PreparationCauses{1 << 21, 1 << 31, ^PreparationCauses(0), all | 1<<31} {
		if err := causes.Validate(); !errors.Is(err, ErrIdentity) {
			t.Fatalf("unknown mask accepted: %d: %v", causes, err)
		}
	}
}

func preparationReasonFixtures() []PreparationReason {
	return []PreparationReason{
		PreparationReasonIdentityConflict, PreparationReasonTransitionPending,
		PreparationReasonPublishedPairUnavailableLive, PreparationReasonActiveWork,
		PreparationReasonStateUnreadable, PreparationReasonPredecessorPairUnavailable,
		PreparationReasonObservationFailed, PreparationReasonEndpointOwnerUnknown,
		PreparationReasonEndpointRefusedLive, PreparationReasonHistoricalTargetNotReady,
	}
}

func TestPreparationFailureReasonsDoNotSupplyCauses(t *testing.T) {
	if err := (PreparationFailure{}).Validate(); err != nil {
		t.Fatalf("zero failure refused: %v", err)
	}
	for _, reason := range preparationReasonFixtures() {
		for _, failure := range []PreparationFailure{
			{Causes: PreparationCauseBinding, Reason: reason}, {Reason: reason, IndependentFault: true},
		} {
			if err := failure.Validate(); err != nil {
				t.Fatalf("classified failure refused: %+v: %v", failure, err)
			}
		}
		if err := (PreparationFailure{Reason: reason}).Validate(); !errors.Is(err, ErrIdentity) {
			t.Fatalf("reason supplied a cause: %s: %v", reason, err)
		}
		if err := (PreparationFailure{Causes: 1 << 31, Reason: reason}).Validate(); !errors.Is(err, ErrIdentity) {
			t.Fatalf("reason blessed unknown cause: %s: %v", reason, err)
		}
	}
	if err := (PreparationFailure{Causes: PreparationCauseBinding, Reason: "unknown"}).Validate(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("unknown reason accepted: %v", err)
	}
}

func TestPreparationFailureStrictJSON(t *testing.T) {
	for _, data := range []string{
		`null`, `{}`, `{"causes":0}`, `{"causes":null,"independent_fault":false}`,
		`{"causes":0,"independent_fault":null}`, `{"causes":0,"reason":null,"independent_fault":false}`,
		`{"causes":0,"independent_fault":false,"extra":true}`,
		`{"causes":4294967296,"independent_fault":false}`,
	} {
		var failure PreparationFailure
		if err := task.DecodeStrict([]byte(data), &failure); err == nil {
			t.Fatalf("invalid failure representation decoded: %s", data)
		}
	}
	for _, failure := range []PreparationFailure{{}, {Causes: PreparationCauseDeadline | PreparationCauseBinding, Reason: PreparationReasonObservationFailed, IndependentFault: true}} {
		data, err := task.MarshalCanonical(failure)
		if err != nil {
			t.Fatal(err)
		}
		var decoded PreparationFailure
		if err = task.DecodeStrict(data, &decoded); err != nil || decoded != failure {
			t.Fatalf("failure round trip: %v", err)
		}
		if err = decoded.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}
