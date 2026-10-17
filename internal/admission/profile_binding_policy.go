package admission

import (
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func validateInspectionTaskBinding(record inspection.RequestRecord, req task.TaskRecord, definition commonprovider.InspectionDefinition) error {
	requestBytes, err := task.MarshalCanonical(req)
	if err != nil || record.TaskSHA256 != task.ComputeSHA256(requestBytes) || record.Binding.DefinitionRevision != definition.Revision {
		return task.ErrEvidenceFault
	}
	return nil
}

func validateStaticInspectionCandidate(record inspection.RequestRecord, definition commonprovider.InspectionDefinition, digest string) error {
	if record.Binding.DefinitionSHA256 != digest || record.Binding.HelperExecutable != definition.Executable || record.Binding.HelperSHA256 != definition.ExecutableSHA256 {
		return task.ErrEvidenceFault
	}
	return nil
}

func ValidatePendingRuntimeCandidate(candidate commonprovider.ProfileCandidate, meta task.MetaRecord) error {
	if candidate.Admit == nil || candidate.Inspection == nil || !candidate.Inspection.RuntimeOnly() {
		return task.ErrIdentityMismatch
	}
	capability, err := commonprovider.RuntimeCapabilityDigest(*candidate.Inspection)
	if err != nil || capability != meta.RuntimePreflightSHA256 || !meta.EnvironmentRecorded ||
		!slices.Equal(candidate.Inspection.Environment, meta.Environment) {
		return task.ErrIdentityMismatch
	}
	return nil
}

func runtimeOnlyInspectionDefinition(definition commonprovider.InspectionDefinition) bool {
	return definition.RuntimeOnly()
}

func runtimeOwnsInspectionExecutable(definition commonprovider.InspectionDefinition) bool {
	return definition.Runtime != nil && definition.Executable == definition.Runtime.Executable && definition.ExecutableSHA256 == definition.Runtime.ExecutableSHA256
}

func matchPreparedProfile(candidate commonprovider.ProfileCandidate, profile commonprovider.PreparedProfile, req task.TaskRecord, meta task.MetaRecord) error {
	if candidate.Inspection != nil && candidate.Inspection.Runtime != nil {
		return profile.MatchesWithRuntimeRefresh(req, meta)
	}
	return profile.Matches(req, meta)
}
