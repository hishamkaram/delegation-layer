package remote

// FailureResponse projects a classified operation error onto the protocol.
func FailureResponse(code string, cause error) *Failure {
	return &Failure{Code: code, Message: cause.Error()}
}
