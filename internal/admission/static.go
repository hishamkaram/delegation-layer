package admission

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func ValidateStatic(options StaticOptions, root string, request task.TaskRecord) error {
	if err := options.Catalog.ValidateRequest(request); err != nil {
		return err
	}
	candidate, err := options.Profiles.Prepare(root, request)
	if err != nil {
		return err
	}
	if err = candidate.ValidateStatePlacement(root); err != nil {
		return err
	}
	_, err = finalizeStaticCandidate(candidate, request)
	return err
}

func finalizeStaticCandidate(candidate commonprovider.ProfileCandidate, request task.TaskRecord) (commonprovider.PreparedProfile, error) {
	if candidate.Inspection != nil {
		return commonprovider.PreparedProfile{}, nil
	}
	return FinalizeCandidate(candidate, request, nil)
}
