package lifetime

// RequirePersistentProcess verifies the resident incarnation. Native SSH logout
// survival on macOS is established by the acceptance gate, not this observation.
func RequirePersistentProcess(expected ProcessRef) error {
	observed, err := ObserveProcess(expected.PID())
	if err != nil {
		return err
	}
	same, err := SameProcess(observed, expected)
	if err != nil {
		return err
	}
	if !same {
		return ErrInvalidRef
	}
	return nil
}
