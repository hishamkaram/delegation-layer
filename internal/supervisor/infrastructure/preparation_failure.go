package infrastructure

// PreparationCauses transports exact classified leaves without importing their
// domain owners. The app owns classification and reconstruction; this immutable
// set neither infers causes from reasons nor grants recovery authority.
type PreparationCauses uint32

const (
	PreparationCausePrivateMigrationRequired PreparationCauses = 1 << iota
	PreparationCausePrivateMigrationBusy
	PreparationCauseReferencedMigrationUnsupported
	PreparationCauseInspectionMigrationRequired
	PreparationCauseInspectionMigrationBusy
	PreparationCauseInspectionMigrationUnsupported
	PreparationCauseTransitionIncomplete
	PreparationCauseNotInitialized
	PreparationCauseEndpointRefused
	PreparationCauseBinding
	PreparationCauseConfiguration
	PreparationCauseUnknown
	PreparationCauseInFlight
	PreparationCauseStatusOperational
	PreparationCauseControlLimit
	PreparationCauseLockBusy
	PreparationCauseDeadline
	PreparationCauseCanceled
	PreparationCauseInfrastructureIdentity
	PreparationCauseTaskIdentity
	PreparationCauseEvidenceFault
)

const preparationKnownCauses = PreparationCausePrivateMigrationRequired |
	PreparationCausePrivateMigrationBusy | PreparationCauseReferencedMigrationUnsupported |
	PreparationCauseInspectionMigrationRequired | PreparationCauseInspectionMigrationBusy |
	PreparationCauseInspectionMigrationUnsupported | PreparationCauseTransitionIncomplete |
	PreparationCauseNotInitialized | PreparationCauseEndpointRefused | PreparationCauseBinding |
	PreparationCauseConfiguration | PreparationCauseUnknown | PreparationCauseInFlight |
	PreparationCauseStatusOperational | PreparationCauseControlLimit | PreparationCauseLockBusy |
	PreparationCauseDeadline | PreparationCauseCanceled | PreparationCauseInfrastructureIdentity |
	PreparationCauseTaskIdentity | PreparationCauseEvidenceFault

func (c PreparationCauses) Validate() error {
	if c & ^preparationKnownCauses != 0 {
		return ErrIdentity
	}
	return nil
}

type PreparationReason string

const (
	PreparationReasonIdentityConflict             PreparationReason = "identity_conflict"
	PreparationReasonTransitionPending            PreparationReason = "transition_pending"
	PreparationReasonPublishedPairUnavailableLive PreparationReason = "published_pair_unavailable_live"
	PreparationReasonActiveWork                   PreparationReason = "active_work"
	PreparationReasonStateUnreadable              PreparationReason = "state_unreadable"
	PreparationReasonPredecessorPairUnavailable   PreparationReason = "predecessor_pair_unavailable"
	PreparationReasonObservationFailed            PreparationReason = "observation_failed"
	PreparationReasonEndpointOwnerUnknown         PreparationReason = "endpoint_owner_unknown"
	PreparationReasonEndpointRefusedLive          PreparationReason = "endpoint_refused_live"
)

func (r PreparationReason) Validate() error {
	switch r {
	case "", PreparationReasonIdentityConflict, PreparationReasonTransitionPending,
		PreparationReasonPublishedPairUnavailableLive, PreparationReasonActiveWork,
		PreparationReasonStateUnreadable, PreparationReasonPredecessorPairUnavailable,
		PreparationReasonObservationFailed, PreparationReasonEndpointOwnerUnknown,
		PreparationReasonEndpointRefusedLive:
		return nil
	default:
		return ErrIdentity
	}
}

// PreparationFailure belongs to one immutable outcome. A zero value means no
// failure. IndependentFault preserves opaque leaves and retained-cleanup
// markers; a reason alone never supplies a missing cause.
type PreparationFailure struct {
	Causes           PreparationCauses `json:"causes"`
	Reason           PreparationReason `json:"reason,omitempty"`
	IndependentFault bool              `json:"independent_fault"`
}

func (f PreparationFailure) Validate() error {
	if err := f.Causes.Validate(); err != nil {
		return err
	}
	if err := f.Reason.Validate(); err != nil {
		return err
	}
	if f.Reason != "" && f.Causes == 0 && !f.IndependentFault {
		return ErrIdentity
	}
	return nil
}
