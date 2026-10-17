package admission

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func readCapabilityEvidence(describe ProviderDescriptionReader, req task.TaskRecord, candidate commonprovider.ProfileCandidate, profile commonprovider.PreparedProfile) (CapabilityEvidence, error) {
	if describe == nil {
		return CapabilityEvidence{}, WithStage("select-provider", task.ErrEvidenceFault)
	}
	description, err := describe(req.Provider)
	if err != nil {
		return CapabilityEvidence{}, WithStage("select-provider", err)
	}
	return CapabilityEvidence{Description: description, Runtime: RuntimeCapabilityFor(candidate, profile)}, nil
}
