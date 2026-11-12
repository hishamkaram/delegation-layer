package task

// ProviderAuthenticationFailureDiagnostic is sanitized interpreter evidence,
// never provider text or a request to change Delegate's configuration.
func ProviderAuthenticationFailureDiagnostic() OutputDiagnostic {
	return OutputDiagnostic{Code: "provider_authentication_failed", Message: "Provider reported an authentication failure. Run the native provider login before starting another task.", Field: "assistant.error"}
}

// HasProviderAuthenticationFailure accepts the canonical, validated diagnostic
// rather than inferring authentication from an error string or a code alone.
func HasProviderAuthenticationFailure(diagnostics []OutputDiagnostic) bool {
	expected := ProviderAuthenticationFailureDiagnostic()
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == expected.Code && diagnostic.Message == expected.Message && diagnostic.Field == expected.Field && ValidateOutputDiagnostic(diagnostic) == nil {
			return true
		}
	}
	return false
}
