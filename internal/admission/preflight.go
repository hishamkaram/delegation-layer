package admission

import (
	"errors"

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
		var placement *statePlacementError
		if errors.As(candidateErr, &placement) {
			result.Stage = PreflightPlacement
		}
		return result, candidateErr
	}
	if candidateErr = candidate.ValidateStatePlacement(root); candidateErr != nil {
		result.Stage = PreflightPlacement
		return result, candidateErr
	}
	profile, candidateErr := finalizeStaticCandidate(candidate, request)
	if candidateErr != nil {
		result.Stage = PreflightStatic
		return result, candidateErr
	}
	result.ProviderExecutable = profile.Plan.Executable
	if candidate.Inspection != nil && candidate.Inspection.Runtime != nil {
		result.ProviderExecutable = candidate.Inspection.Runtime.Executable
	}
	result.Stage = PreflightReady
	return result, nil
}
