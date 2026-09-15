//go:build !darwin

package claude

import "fmt"

// inspectLegacyAPIKeyAbsence is unavailable where the pinned Claude native
// keychain backend cannot be inspected without a different platform contract.
func inspectLegacyAPIKeyAbsence(profileEnvironment) error {
	return fmt.Errorf("%w: legacy Claude API-key inspection is not certified on this platform", ErrUnsupportedProfile)
}
