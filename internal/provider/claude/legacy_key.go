package claude

// legacyAPIKeyInspection records the platform-specific legacy API-key check
// without making the shared profile code reason about a platform-specific
// error return. Verified is true only when the native check proved absence.
type legacyAPIKeyInspection struct {
	Verified bool
	Err      error
}

func validateLegacyAPIKeyInspection(result legacyAPIKeyInspection) error {
	if result.Verified {
		return nil
	}
	if result.Err != nil {
		return result.Err
	}
	return unsupportedNativeFacts()
}
