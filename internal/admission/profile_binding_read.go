package admission

import (
	"context"
	"errors"
	"os"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// validateInspectionCandidate binds the reconstructed compiled definition and
// full immutable task to existing evidence. Runtime-only provider identity is
// refreshed by the current capability probe; the inspection worker identity
// remains separately bound. It never reads native credentials.
func ValidateInspectionCandidate(ctx context.Context, operation *inspection.Operation, req task.TaskRecord, candidate commonprovider.ProfileCandidate) error {
	if operation == nil || candidate.Inspection == nil {
		return task.ErrEvidenceFault
	}
	definition, digest, err := candidate.Inspection.Snapshot()
	if err != nil {
		return task.ErrEvidenceFault
	}
	record := operation.Request()
	if err = validateInspectionTaskBinding(record, req, definition); err != nil {
		return err
	}
	if definition.Runtime == nil {
		return validateStaticInspectionCandidate(record, definition, digest)
	}
	if runtimeOnlyInspectionDefinition(definition) {
		return validateRuntimeInspectionCandidate(ctx, operation, record, req, definition, digest)
	}
	if err = validateMixedInspectionCandidate(ctx, operation, record, definition, digest); err != nil {
		return err
	}
	return nil
}

func validateMixedInspectionCandidate(ctx context.Context, operation *inspection.Operation, record inspection.RequestRecord, definition commonprovider.InspectionDefinition, digest string) error {
	if !runtimeOwnsInspectionExecutable(definition) && (record.Binding.HelperExecutable != definition.Executable || record.Binding.HelperSHA256 != definition.ExecutableSHA256) {
		return task.ErrEvidenceFault
	}
	if !slices.Equal(record.Binding.Environment, definition.Environment) {
		return task.ErrEvidenceFault
	}
	if record.Binding.CapabilitySHA256 != "" {
		capability, capabilityErr := commonprovider.RuntimeInspectionContractDigest(definition)
		if capabilityErr != nil || capability != record.Binding.CapabilitySHA256 {
			return task.ErrEvidenceFault
		}
	}
	if record.Binding.DefinitionSHA256 == digest {
		return nil
	}
	return validateHistoricalRuntimeDefinition(ctx, operation, record.Binding.DefinitionSHA256, definition)
}

func validateHistoricalRuntimeDefinition(ctx context.Context, operation *inspection.Operation, expectedDigest string, definition commonprovider.InspectionDefinition) error {
	proof, err := operation.ReadProofContext(ctx)
	if err != nil {
		return task.ErrEvidenceFault
	}
	facts, err := commonprovider.DecodeInspectionFacts(proof)
	if err != nil || facts.Runtime == nil {
		return task.ErrEvidenceFault
	}
	if runtimeOnlyInspectionDefinition(definition) {
		if facts.Native != nil {
			return task.ErrEvidenceFault
		}
		capability, capabilityErr := commonprovider.RuntimeCapabilityDigest(definition)
		if capabilityErr == nil && facts.Runtime.CapabilitySHA256 == capability {
			return nil
		}
	}
	replayed := definition
	runtime := *definition.Runtime
	runtime.Executable = facts.Runtime.Executable
	runtime.ExecutableSHA256 = facts.Runtime.SHA256
	replayed.Runtime = &runtime
	if runtimeOwnsInspectionExecutable(definition) {
		replayed.Executable = facts.Runtime.Executable
		replayed.ExecutableSHA256 = facts.Runtime.SHA256
	}
	if _, digest, snapshotErr := replayed.Snapshot(); snapshotErr != nil || digest != expectedDigest {
		return task.ErrEvidenceFault
	}
	return nil
}

func validateRuntimeInspectionCandidate(ctx context.Context, operation *inspection.Operation, record inspection.RequestRecord, req task.TaskRecord, definition commonprovider.InspectionDefinition, digest string) error {
	if definition.Directory != req.CanonicalCwd || !slices.Equal(record.Binding.Environment, definition.Environment) {
		return task.ErrEvidenceFault
	}
	capabilitySHA, err := commonprovider.RuntimeCapabilityDigest(definition)
	if err != nil {
		return task.ErrEvidenceFault
	}
	if record.Binding.CapabilitySHA256 != "" {
		if record.Binding.CapabilitySHA256 != capabilitySHA {
			return task.ErrEvidenceFault
		}
		return nil
	}
	if record.Binding.DefinitionSHA256 == digest {
		return nil
	}
	if runtimeOnlyInspectionDefinition(definition) {
		if _, _, completionErr := operation.ReadCompletedContext(ctx); errors.Is(completionErr, os.ErrNotExist) {
			// A markerless historical journal has no persisted capability
			// contract. Its current worker is the fresh supervised probe, so
			// allow the probe to establish the current contract in its facts.
			return nil
		}
	}
	// Older runtime journals predate CapabilitySHA256. Reconstruct the
	// historical definition using the provider identity in the sealed proof so
	// a changed help/flag contract cannot become authoritative merely because
	// the current probe succeeds.
	if err = validateHistoricalRuntimeDefinition(ctx, operation, record.Binding.DefinitionSHA256, definition); err != nil {
		return task.ErrEvidenceFault
	}
	return nil
}
