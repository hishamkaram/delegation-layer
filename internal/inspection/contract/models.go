package contract

import (
	"time"

	provider "github.com/hishamkaram/delegation-layer/internal/provider/contract"
)

func InspectionFactsLimit(limits []int) int {
	if len(limits) > 0 {
		return limits[0]
	}
	return MaxProjectedFacts
}

const (
	// ModelDiscoveryTimeout is the finite window for native model metadata
	// initialization. Provider CLIs may populate a remote model/cache catalog on
	// their first run, so this is longer than the ordinary capability probe while
	// remaining bounded and supervisor-owned.
	ModelDiscoveryTimeout = 5 * time.Minute
	// legacyModelDiscoveryTimeout remains valid when recovering a v1 journal
	// written before the cold-start allowance was widened.
	legacyModelDiscoveryTimeout = time.Minute
)

// Model metadata initialization loads the harness's configured integrations;
// measured cold starts can exceed the ordinary capability probe's deadline.
// The revision keeps historical admission deadlines unchanged.
func InspectionTimeout(binding Binding) time.Duration {
	if binding.DefinitionRevision == provider.ModelsRevision {
		return ModelDiscoveryTimeout
	}
	return AdmissionTimeout
}

func inspectionDeadlineMatches(binding Binding, duration time.Duration) bool {
	if duration == InspectionTimeout(binding) {
		return true
	}
	if binding.DefinitionRevision == provider.ModelsRevision {
		return duration == legacyModelDiscoveryTimeout
	}
	return duration == legacyAdmissionTimeout
}

// TimeoutForRevision is the finite inspection budget selected by a persisted
// definition revision. Fresh admission probes get the current queue-inclusive
// allowance; persisted requests retain their recorded historical deadline.
func TimeoutForRevision(revision string) time.Duration {
	return InspectionTimeout(Binding{DefinitionRevision: revision})
}
