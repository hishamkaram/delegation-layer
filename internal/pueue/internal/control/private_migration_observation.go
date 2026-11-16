package control

import "path/filepath"

// PrivateMigrationPhase is a passive projection of the migration owner's
// validated journal. It grants no transition or launch authority.
type PrivateMigrationPhase string

const (
	PrivateMigrationAccepted                      PrivateMigrationPhase = "accepted"
	PrivateMigrationPredecessorShutdownRequested  PrivateMigrationPhase = "predecessor_shutdown_requested"
	PrivateMigrationPredecessorShutdownDispatched PrivateMigrationPhase = "predecessor_shutdown_dispatched"
	PrivateMigrationPredecessorStopped            PrivateMigrationPhase = "predecessor_stopped"
)

// ObservePrivateMigrationPhase reads only the existing migration journal. A
// caller making a stable decision retains its existing SH or EX generation
// lease; this observer creates no lock or state itself.
func ObservePrivateMigrationPhase(stateRoot string) (PrivateMigrationPhase, bool, error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return "", false, ErrConfiguration
	}
	record, found, err := loadPrivateMigrationRecord(filepath.Join(stateRoot, privateSupervisorDirectory))
	if err != nil {
		return "", false, privateRecoveryFailure("state_unreadable", err)
	}
	if !found {
		return "", false, nil
	}
	switch {
	case record.LegacyStopped:
		return PrivateMigrationPredecessorStopped, true, nil
	case record.LegacyShutdownDispatched:
		return PrivateMigrationPredecessorShutdownDispatched, true, nil
	case record.LegacyShutdownRequested:
		return PrivateMigrationPredecessorShutdownRequested, true, nil
	default:
		return PrivateMigrationAccepted, true, nil
	}
}
