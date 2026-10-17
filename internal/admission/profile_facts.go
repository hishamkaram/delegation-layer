package admission

import (
	"bytes"
	"encoding/json"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func factsForCurrentRuntime(candidate commonprovider.ProfileCandidate, facts json.RawMessage) (json.RawMessage, error) {
	if candidate.Inspection == nil || candidate.Inspection.Runtime == nil || len(facts) == 0 {
		return facts, nil
	}
	decoded, err := commonprovider.DecodeInspectionFacts(facts)
	if err != nil || decoded.Runtime == nil {
		return nil, task.ErrEvidenceFault
	}
	runtime := *decoded.Runtime
	runtime.Executable = candidate.Inspection.Runtime.Executable
	runtime.SHA256 = candidate.Inspection.Runtime.ExecutableSHA256
	return commonprovider.EncodeInspectionFacts(runtime, decoded.Native)
}

// inspectionFactsMatch compares the canonical nonsecret projection returned
// by admission with the fresh projection at the ordinary start boundary. For
// runtime-only profiles it excludes the provider identity, which is replaced
// by the current launch plan; all native capability facts remain exact.
func inspectionFactsMatch(stored, fresh json.RawMessage) bool {
	return inspectionFactsMatchForDefinition(stored, fresh, true)
}

func inspectionFactsMatchForDefinition(stored, fresh json.RawMessage, runtimeEnabled bool) bool {
	if !runtimeEnabled {
		canonicalStored, storedErr := canonicalInspectionFacts(stored)
		canonicalFresh, freshErr := canonicalInspectionFacts(fresh)
		return storedErr == nil && freshErr == nil && bytes.Equal(canonicalStored, canonicalFresh)
	}
	storedFacts, storedErr := commonprovider.DecodeInspectionFacts(stored)
	freshFacts, freshErr := commonprovider.DecodeInspectionFacts(fresh)
	if storedErr != nil || freshErr != nil || (storedFacts.Runtime == nil) != (freshFacts.Runtime == nil) {
		return false
	}
	if storedFacts.Runtime != nil {
		return canonicalNativeFactsEqual(storedFacts.Native, freshFacts.Native)
	}
	canonicalStored, storedErr := canonicalInspectionFacts(storedFacts.Native)
	canonicalFresh, freshErr := canonicalInspectionFacts(freshFacts.Native)
	return storedErr == nil && freshErr == nil && bytes.Equal(canonicalStored, canonicalFresh)
}

func canonicalNativeFactsEqual(stored, fresh json.RawMessage) bool {
	if len(stored) == 0 || len(fresh) == 0 {
		return len(stored) == 0 && len(fresh) == 0
	}
	canonicalStored, storedErr := canonicalInspectionFacts(stored)
	canonicalFresh, freshErr := canonicalInspectionFacts(fresh)
	return storedErr == nil && freshErr == nil && bytes.Equal(canonicalStored, canonicalFresh)
}

func canonicalInspectionFacts(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || task.ValidateJSONStructure(raw) != nil {
		return nil, task.ErrEvidenceFault
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, task.ErrEvidenceFault
	}
	canonical, err := task.MarshalCanonical(object)
	if err != nil {
		return nil, task.ErrEvidenceFault
	}
	return canonical, nil
}
