package admission

import (
	"bytes"
	"encoding/json"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// factsForCurrentRuntime projects saved admission facts onto the current
// candidate's installed executable in memory. The saved proof stays unchanged.
// Its reported version described the executable observed at admission, so it
// is not presented as an observation of the current one.
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
	runtime.Version = ""
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

// inspectionFactsError is a sanitized start-boundary refusal of a fresh
// inspection. It keeps the evidence-fault identity and publishes only its
// fixed message, never provider bytes or a provider error.
type inspectionFactsError struct{ message string }

func (e *inspectionFactsError) Error() string { return e.message }

func (e *inspectionFactsError) Unwrap() error { return task.ErrEvidenceFault }

var (
	errInspectionFactsChanged   = &inspectionFactsError{message: "provider inspection facts changed after this task was admitted, so Delegate did not launch it"}
	errInspectionFactsMalformed = &inspectionFactsError{message: "provider inspection facts are malformed, so Delegate did not launch this task"}
	errInspectionFactsDecision  = &inspectionFactsError{message: "the provider's decision on changed inspection facts violated its contract, so Delegate did not launch this task"}
)

// refreshedInspectionFactsError binds a fresh native projection to the
// admission proof: canonical equality, or the candidate's own decision on a
// validated difference. Malformed evidence never reaches that decision, and
// the decision cannot relax any other binding.
func refreshedInspectionFactsError(candidate commonprovider.ProfileCandidate, stored, fresh json.RawMessage) error {
	runtimeEnabled := candidate.Inspection.Runtime != nil
	if inspectionFactsMatchForDefinition(stored, fresh, runtimeEnabled) {
		return nil
	}
	storedNative, freshNative, err := differingNativeProjections(stored, fresh, runtimeEnabled)
	if err != nil {
		return err
	}
	if !runtimeEnabled || candidate.AcceptRefreshedNativeFacts == nil {
		return errInspectionFactsChanged
	}
	return refreshedNativeFactsDecision(candidate.AcceptRefreshedNativeFacts(storedNative, freshNative))
}

// differingNativeProjections returns the canonical projections of two
// inspection envelopes that are not equal. With a runtime probe, both
// envelopes must carry runtime facts and a native object.
func differingNativeProjections(stored, fresh json.RawMessage, runtimeEnabled bool) (json.RawMessage, json.RawMessage, error) {
	if runtimeEnabled {
		storedFacts, storedErr := commonprovider.DecodeInspectionFacts(stored)
		freshFacts, freshErr := commonprovider.DecodeInspectionFacts(fresh)
		if storedErr != nil || freshErr != nil || storedFacts.Runtime == nil || freshFacts.Runtime == nil {
			return nil, nil, errInspectionFactsMalformed
		}
		stored, fresh = storedFacts.Native, freshFacts.Native
	}
	canonicalStored, storedErr := canonicalInspectionFacts(stored)
	canonicalFresh, freshErr := canonicalInspectionFacts(fresh)
	if storedErr != nil || freshErr != nil {
		return nil, nil, errInspectionFactsMalformed
	}
	return canonicalStored, canonicalFresh, nil
}

// refreshedNativeFactsDecision maps the candidate decision to a fixed
// evidence fault. Anything but a whole refusal is a contract fault, so no
// provider sentinel or text crosses the start boundary.
func refreshedNativeFactsDecision(err error) error {
	if err == nil {
		return nil
	}
	reason, ok := commonprovider.NativeFactsRefusal(err)
	switch {
	case !ok:
		return errInspectionFactsDecision
	case reason == commonprovider.NativeFactsInvalid:
		return errInspectionFactsMalformed
	default:
		return errInspectionFactsChanged
	}
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
