package admission

import (
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func Preflight(options StaticOptions, root string, request task.TaskRecord) (result PreflightResult, resultErr error) {
	registration, lookupErr := options.Catalog.Lookup(request.Provider)
	if lookupErr != nil {
		return result, lookupErr
	}
	if validationErr := options.Catalog.ValidateRequest(request); validationErr != nil {
		return result, validationErr
	}
	result.Description = &registration.Description
	candidate, candidateErr := options.Profiles.Prepare(root, request)
	if candidateErr != nil {
		result.Stage = PreflightStatic
		return result, candidateErr
	}
	if candidateErr = candidate.ValidateStatePlacement(root); candidateErr != nil {
		result.Stage = PreflightPlacement
		return result, candidateErr
	}
	if candidateErr = finalizeStaticCandidate(candidate, request); candidateErr != nil {
		result.Stage = PreflightStatic
		return result, candidateErr
	}
	result.Stage = PreflightReady
	return result, nil
}
