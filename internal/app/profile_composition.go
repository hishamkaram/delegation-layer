package app

import (
	"context"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type admissionCandidateSource struct{ dependencies Dependencies }

func (source admissionCandidateSource) Current(request task.TaskRecord) (commonprovider.ProfileCandidate, error) {
	return source.dependencies.normalized().PrepareCandidate(request)
}

func (source admissionCandidateSource) ForTask(request task.TaskRecord, meta *task.MetaRecord) (commonprovider.ProfileCandidate, bool, error) {
	normalized := source.dependencies.normalized()
	if meta == nil || !normalized.useCatalogExistingFor(request) {
		candidate, err := normalized.normalized().PrepareCandidate(request)
		return candidate, false, err
	}
	candidate, err := normalized.Catalog.ExistingCandidate(request, *meta)
	return candidate, true, err
}

func admissionProfiles(deps Dependencies) admissioncore.Profiles {
	return admissioncore.NewProfiles(admissionCandidateSource{dependencies: deps}, func(ctx context.Context, root string) (*taskdir.Store, error) {
		return taskdir.OpenStoreWithPredicatesContext(ctx, root, deps.normalized().storeDependencies().registry())
	})
}
