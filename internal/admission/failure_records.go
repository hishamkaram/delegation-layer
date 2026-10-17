package admission

type StageError struct {
	stage string
	err   error
}

type InspectionFailureContextError struct {
	err           error
	taskID        string
	deadlineState string
}
