package admission

func boundedSelectionReason(provider string, err error) string {
	if err == nil {
		return provider + ": unavailable"
	}
	message := err.Error()
	if len(message) > 240 {
		message = message[:240] + "..."
	}
	return provider + ": " + message
}
