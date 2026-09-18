package claude

// legacyAPIKeyInspection records the optional legacy API-key check without
// making the shared profile code reason about a platform-specific error return.
// Verified is true when the native check proved absence or when that optional
// API is unavailable and authentication is left to the provider CLI.
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
