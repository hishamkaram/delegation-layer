package app

import (
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func creationCommandResult(response Response, provider string, created tasklifecycle.CreationResult) commandResult {
	if created.Capability != nil {
		capability := describeRuntimeCapability(provider, created.Capability.Description, created.Capability.Runtime)
		response.Capability = &capability
	}
	switch created.TaskRecord {
	case tasklifecycle.CreationRecordUnobserved:
		// The caller may already have observed a record before falling back.
	case tasklifecycle.CreationRecordUnknown:
		response.TaskRecord = "unknown"
	case tasklifecycle.CreationRecordCreated:
		response.TaskRecord = "created"
	}
	if created.Submission != nil {
		result := submissionCommandResult(response, *created.Submission)
		if result.err != nil {
			result.err = created.Err()
			result.response.setError(result.err)
		}
		return result
	}
	fallback := 1
	if created.Stage == tasklifecycle.CreationAdmission || created.Stage == tasklifecycle.CreationProfile {
		fallback = 2
	}
	code := classifyCode(created.Err(), fallback)
	if created.Stage == tasklifecycle.CreationCapture {
		code = 1
	}
	return failed(response, created.Err(), code)
}
