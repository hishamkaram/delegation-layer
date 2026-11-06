package app

import (
	"context"
	"errors"
	"reflect"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type infrastructureCauseMapping struct {
	cause infrastructure.PreparationCauses
	err   error
}

// Domain owners retain sentinel identity. This local immutable table owns only
// the wire projection; neither display reasons nor Is aliases add causes.
func infrastructurePreparationMappings() [21]infrastructureCauseMapping {
	return [21]infrastructureCauseMapping{
		{infrastructure.PreparationCausePrivateMigrationRequired, pueue.ErrPrivateSupervisorMigrationRequired},
		{infrastructure.PreparationCausePrivateMigrationBusy, pueue.ErrPrivateSupervisorMigrationBusy},
		{infrastructure.PreparationCauseHistoricalTargetUnsupported, pueue.ErrHistoricalTargetUnsupported},
		{infrastructure.PreparationCauseInspectionMigrationRequired, inspection.ErrGroupMigrationRequired},
		{infrastructure.PreparationCauseInspectionMigrationBusy, inspection.ErrGroupMigrationBusy},
		{infrastructure.PreparationCauseInspectionMigrationUnsupported, inspection.ErrGroupMigrationUnsupported},
		{infrastructure.PreparationCauseTransitionIncomplete, pueue.ErrPrivateSupervisorTransitionIncomplete},
		{infrastructure.PreparationCauseNotInitialized, pueue.ErrPrivateSupervisorNotInitialized},
		{infrastructure.PreparationCauseEndpointRefused, pueue.ErrPrivateEndpointRefused},
		{infrastructure.PreparationCauseBinding, pueue.ErrBinding},
		{infrastructure.PreparationCauseConfiguration, pueue.ErrConfiguration},
		{infrastructure.PreparationCauseUnknown, pueue.ErrUnknown},
		{infrastructure.PreparationCauseInFlight, pueue.ErrInFlight},
		{infrastructure.PreparationCauseStatusOperational, pueue.ErrStatusOperational},
		{infrastructure.PreparationCauseControlLimit, pueue.ErrControlLimit},
		{infrastructure.PreparationCauseLockBusy, task.ErrLockBusy},
		{infrastructure.PreparationCauseDeadline, context.DeadlineExceeded},
		{infrastructure.PreparationCauseCanceled, context.Canceled},
		{infrastructure.PreparationCauseInfrastructureIdentity, infrastructure.ErrIdentity},
		{infrastructure.PreparationCauseTaskIdentity, task.ErrIdentityMismatch},
		{infrastructure.PreparationCauseEvidenceFault, task.ErrEvidenceFault},
	}
}

func exactInfrastructurePreparationCause(err error) infrastructure.PreparationCauses {
	value := reflect.ValueOf(err)
	if !value.IsValid() || !value.Comparable() {
		return 0
	}
	for _, mapping := range infrastructurePreparationMappings() {
		// Exact sentinel identity is required; Is also accepts overlapping aliases.
		canonical := reflect.ValueOf(mapping.err)
		if canonical.IsValid() && canonical.Comparable() && value.Type() == canonical.Type() && value.Equal(canonical) {
			return mapping.cause
		}
	}
	return 0
}

// Classification happens after canonical host cleanup. Visit the entire error
// graph so neither an earlier known leaf nor a wrapper hides retained owners.
func (infrastructureEntryHost) ClassifyPreparation(err error) infrastructure.PreparationFailure {
	if err == nil {
		return infrastructure.PreparationFailure{}
	}
	failure := infrastructure.PreparationFailure{}
	task.AnyErrorNode(err, func(node error) bool {
		cause, independent := infrastructurePreparationNodeFailure(node)
		failure.Causes |= cause
		failure.IndependentFault = failure.IndependentFault || independent
		return false
	})
	// The existing presentation policy selects the reason. Validate its closed
	// representation separately; a new unmapped reason remains a hard fault.
	reason := infrastructure.PreparationReason(supervisorFailureReason(err))
	if reason.Validate() == nil {
		failure.Reason = reason
	} else {
		failure.IndependentFault = true
	}
	return failure
}

func infrastructurePreparationNodeFailure(node error) (infrastructure.PreparationCauses, bool) {
	_, independent := node.(interface{ RetainsResidentCleanup() })
	if joined, ok := node.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		for _, child := range children {
			independent = independent || child == nil
		}
		if len(children) > 0 {
			return 0, independent
		}
	}
	if errors.Unwrap(node) != nil {
		return 0, independent
	}
	cause := exactInfrastructurePreparationCause(node)
	return cause, independent || cause == 0
}

func infrastructurePreparationFailureError(failure infrastructure.PreparationFailure) error {
	var causes []error
	for _, mapping := range infrastructurePreparationMappings() {
		if failure.Causes&mapping.cause != 0 {
			causes = append(causes, mapping.err)
		}
	}
	if failure.IndependentFault {
		causes = append(causes, infrastructure.ErrOwnerRequest)
	}
	err := errors.Join(causes...)
	if failure.Reason != "" {
		return &pueue.PrivateRecoveryError{ReasonCode: string(failure.Reason), Err: err}
	}
	return err
}

// Only a verified outcome enters this projection. Selection, transport,
// readback and Close errors stay separate until the routing caller joins them.
func infrastructurePreparationOutcomeError(outcome infrastructure.PreparationOutcome) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	cause := infrastructurePreparationFailureError(outcome.Failure)
	switch outcome.Condition {
	case "":
		return nil
	case infrastructure.PreparationOK:
		if cause != nil {
			return errors.Join(infrastructure.ErrIdentity, cause)
		}
		return nil
	case infrastructure.PreparationPending:
		return errors.Join(cause, infrastructure.ErrOwnerPending)
	case infrastructure.PreparationBusy:
		if cause == nil {
			return infrastructure.ErrOwnerBusy
		}
		return cause
	case infrastructure.PreparationIncompatible:
		return errors.Join(cause, infrastructure.ErrOwnerIncompatible)
	case infrastructure.PreparationRefused, infrastructure.PreparationFault:
		if cause == nil {
			return infrastructure.ErrOwnerRequest
		}
		return cause
	case infrastructure.PreparationTimeout:
		return errors.Join(cause, infrastructure.ErrOwnerRequest, context.DeadlineExceeded)
	}
	return infrastructure.ErrIdentity
}
