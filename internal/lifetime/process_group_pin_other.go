//go:build !darwin

package lifetime

// RunProcessGroupPinMode is a no-op where the Darwin process-group backend is
// unavailable.
func RunProcessGroupPinMode([]string) (int, bool) { return 0, false }
