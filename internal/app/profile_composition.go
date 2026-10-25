package app

import (
	"context"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"

	admissioncore "github.com/hishamkaram/delegation-layer/internal/admission"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type admissionCandidateSource struct{ dependencies Dependencies }

func (source admissionCandidateSource) Current(request task.TaskRecord, environment []string) (commonprovider.ProfileCandidate, error) {
	return source.dependencies.normalized().PrepareCandidate(request, environment)
}

func (source admissionCandidateSource) ForTask(request task.TaskRecord, meta *task.MetaRecord, environment []string) (commonprovider.ProfileCandidate, bool, error) {
	normalized := source.dependencies.normalized()
	if meta == nil || !normalized.useCatalogExistingFor(request) {
		candidate, err := normalized.normalized().PrepareCandidate(request, environment)
		return candidate, false, err
	}
	candidate, err := normalized.Catalog.ExistingCandidate(request, *meta, environment)
	return candidate, true, err
}

// admissionProfiles binds the composition's captured launch scope.
func admissionProfiles(deps Dependencies) admissioncore.Profiles {
	return profilesForEnvironment(deps, deps.Environment)
}

func profilesForEnvironment(deps Dependencies, environment []string) admissioncore.Profiles {
	return admissioncore.NewProfiles(admissionCandidateSource{dependencies: deps}, func(ctx context.Context, root string) (*taskdir.Store, error) {
		return taskdir.OpenStoreWithPredicatesContext(ctx, root, deps.normalized().storeDependencies().registry())
	}, environment)
}

// scopedDependencies selects a saved launch scope over the captured one with
// the canonical Profiles rule, at the point where the former process-level
// swap applied it. Every later consumer of this composition reads that scope.
func scopedDependencies(deps Dependencies, values []string, recorded bool) (Dependencies, error) {
	profiles, err := admissionProfiles(deps).ForRecord(values, recorded)
	if err != nil {
		return Dependencies{}, err
	}
	deps.Environment = profiles.Environment()
	return deps, nil
}
