package workspaces

// AdmissionCleanupError retains the exact admission whose inner use-owner,
// resource or owned root admission could not close. The outer creator keeps
// its borrowed root lease and store open until Admission().Close succeeds.
// This handle grants cleanup ownership, never another admission or launch.
type AdmissionCleanupError struct {
	admission *Admission
	cause     error
}

func (e *AdmissionCleanupError) Error() string {
	return "workspace admission cleanup unresolved: " + e.cause.Error()
}

func (e *AdmissionCleanupError) Unwrap() error         { return e.cause }
func (e *AdmissionCleanupError) Admission() *Admission { return e.admission }

// CleanupPending describes unresolved ownership, never admission permission.
func (m *Admission) CleanupPending() bool {
	if m == nil {
		return false
	}
	if m.storeCleanupPending() {
		return true
	}
	for _, child := range m.childOwners {
		if child.CleanupPending() {
			return true
		}
	}
	return m.owner.CleanupPending() || m.resource != nil || m.rootAdmission.CleanupPending()
}
