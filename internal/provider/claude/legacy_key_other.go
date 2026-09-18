//go:build !darwin

package claude

// inspectLegacyAPIKeyAbsence is unavailable where the Claude native
// keychain backend cannot be inspected. Authentication remains owned by the
// Claude CLI, so this optional legacy check is treated as absent.
func inspectLegacyAPIKeyAbsence(profileEnvironment) error {
	return nil
}

func inspectLegacyAPIKey(profileEnvironment) legacyAPIKeyInspection {
	return legacyAPIKeyInspection{Verified: true}
}
