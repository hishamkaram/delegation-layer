package infrastructure

type ManualMigrationState string

const (
	ManualMigrationNotInitialized ManualMigrationState = "not_initialized"
	ManualMigrationMigrated       ManualMigrationState = "migrated"
	ManualMigrationBlocked        ManualMigrationState = "blocked"
	ManualMigrationUnavailable    ManualMigrationState = "unavailable"
)

type ManualMigrationLegacy string

const (
	ManualMigrationLegacyUnknown    ManualMigrationLegacy = "unknown"
	ManualMigrationLegacyNotPresent ManualMigrationLegacy = "not_present"
	ManualMigrationLegacyNotManaged ManualMigrationLegacy = "not_managed"
	ManualMigrationLegacyRecovered  ManualMigrationLegacy = "recovered"
)

type ManualMigrationActive string

const (
	ManualMigrationActiveUnknown ManualMigrationActive = "unknown"
	ManualMigrationActiveNone    ManualMigrationActive = "none"
	ManualMigrationActivePresent ManualMigrationActive = "present"
)

// ManualMigrationReport is an immutable observation, not authority. The outcome
// owns failure data; this report contains only the observed migration state.
type ManualMigrationReport struct {
	State  ManualMigrationState  `json:"state"`
	Legacy ManualMigrationLegacy `json:"legacy"`
	Active ManualMigrationActive `json:"active"`
}

func (r ManualMigrationReport) Validate() error {
	switch r.State {
	case ManualMigrationNotInitialized:
		if r.Legacy == ManualMigrationLegacyNotPresent && r.Active == ManualMigrationActiveNone {
			return nil
		}
	case ManualMigrationMigrated:
		return r.validateMigrated()
	case ManualMigrationBlocked:
		if r.Legacy == ManualMigrationLegacyUnknown && r.Active == ManualMigrationActivePresent {
			return nil
		}
	case ManualMigrationUnavailable:
		if r.Legacy == ManualMigrationLegacyUnknown && r.Active == ManualMigrationActiveUnknown {
			return nil
		}
	default:
		return ErrIdentity
	}
	return ErrIdentity
}

func (r ManualMigrationReport) validateMigrated() error {
	switch r.Legacy {
	case ManualMigrationLegacyNotManaged:
		if r.Active == ManualMigrationActiveUnknown {
			return nil
		}
	case ManualMigrationLegacyNotPresent:
		if r.Active == ManualMigrationActiveNone || r.Active == ManualMigrationActivePresent {
			return nil
		}
	case ManualMigrationLegacyRecovered:
		if r.Active == ManualMigrationActiveNone {
			return nil
		}
	case ManualMigrationLegacyUnknown:
		return ErrIdentity
	}
	return ErrIdentity
}

// ValidateCondition permits completed observations to survive a late failure.
// Operation-specific requirements remain with the wire consumer.
func (r ManualMigrationReport) ValidateCondition(condition PreparationCondition) error {
	if err := condition.Validate(); err != nil {
		return err
	}
	if r == (ManualMigrationReport{}) {
		return nil
	}
	if err := r.Validate(); err != nil {
		return err
	}
	switch condition {
	case PreparationOK:
		if r.State == ManualMigrationNotInitialized || r.State == ManualMigrationMigrated {
			return nil
		}
	case PreparationFault, PreparationTimeout:
		return nil
	case PreparationPending, PreparationBusy, PreparationIncompatible, PreparationRefused:
		return ErrIdentity
	}
	return ErrIdentity
}
