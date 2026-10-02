package app

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func prepareCandidate(deps Dependencies, root string, req task.TaskRecord) (commonprovider.ProfileCandidate, error) {
	candidate, err := deps.normalized().PrepareCandidate(req)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if candidate.Directory != req.CanonicalCwd || candidate.Finalize == nil {
		return commonprovider.ProfileCandidate{}, ErrProfileUnavailable
	}
	if err = candidate.ValidateStatePlacement(root); err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if candidate.Inspection != nil {
		definition, _, snapshotErr := candidate.Inspection.Snapshot()
		if snapshotErr != nil {
			return commonprovider.ProfileCandidate{}, snapshotErr
		}
		candidate.Inspection = &definition
	}
	return candidate, nil
}

func finalizeCandidate(candidate commonprovider.ProfileCandidate, req task.TaskRecord, facts json.RawMessage) (PreparedProfile, error) {
	if candidate.Finalize == nil || (candidate.Inspection != nil && len(facts) == 0) {
		return PreparedProfile{}, ErrProfileUnavailable
	}
	profile, err := candidate.Finalize(facts, time.Now())
	if err != nil {
		return PreparedProfile{}, err
	}
	if err = profile.Validate(req); err != nil {
		return PreparedProfile{}, err
	}
	if profile.Plan.Directory != candidate.Directory || !slices.Equal(profile.WritableRoots, candidate.WritableRoots) {
		return PreparedProfile{}, ErrProfileUnavailable
	}
	if profile.Plan.ExecutableSHA256 != "" && profile.ObservedVersion != "" {
		profile.Plan.ProviderRuntime = task.ProviderRuntimeIdentity{
			Executable: profile.Plan.Executable,
			Version:    profile.ObservedVersion,
			SHA256:     profile.Plan.ExecutableSHA256,
		}
	}
	return profile, nil
}

func refreshAdmissionCandidate(deps Dependencies, root string, req task.TaskRecord, admitted commonprovider.ProfileCandidate) (commonprovider.ProfileCandidate, error) {
	if admitted.Inspection == nil || admitted.Inspection.Runtime == nil {
		return admitted, nil
	}
	current, err := prepareCandidate(deps, root, req)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if current.Inspection == nil || current.Inspection.Runtime == nil {
		return commonprovider.ProfileCandidate{}, task.ErrEvidenceFault
	}
	if current.Directory != admitted.Directory || !slices.Equal(current.WritableRoots, admitted.WritableRoots) {
		return commonprovider.ProfileCandidate{}, task.ErrEvidenceFault
	}
	admittedCapability, err := commonprovider.RuntimeInspectionContractDigest(*admitted.Inspection)
	if err != nil {
		return commonprovider.ProfileCandidate{}, task.ErrEvidenceFault
	}
	currentCapability, err := commonprovider.RuntimeInspectionContractDigest(*current.Inspection)
	if err != nil || currentCapability != admittedCapability {
		return commonprovider.ProfileCandidate{}, task.ErrEvidenceFault
	}
	return current, nil
}

// validateInspectionCandidate binds the reconstructed compiled definition and
// full immutable task to existing evidence. Runtime-only provider identity is
// refreshed by the current capability probe; the inspection worker identity
// remains separately bound. It never reads native credentials.
func validateInspectionCandidate(ctx context.Context, operation *inspection.Operation, req task.TaskRecord, candidate commonprovider.ProfileCandidate) error {
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

func storedInspectionFacts(deps Dependencies, root string, req task.TaskRecord, meta task.MetaRecord, candidate commonprovider.ProfileCandidate) (facts json.RawMessage, resultErr error) {
	return storedInspectionFactsContext(context.Background(), deps, root, req, meta, candidate)
}

func storedInspectionFactsContext(ctx context.Context, deps Dependencies, root string, req task.TaskRecord, meta task.MetaRecord, candidate commonprovider.ProfileCandidate) (facts json.RawMessage, resultErr error) {
	store, err := taskdir.OpenStoreWithPredicatesContext(ctx, root, deps.normalized().storeDependencies().registry())
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	operation, err := inspection.LoadOperationContext(ctx, store, req.TaskID)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if err = validateInspectionCandidate(ctx, operation, req, candidate); err != nil {
		return nil, err
	}
	if !task.SameSupervisorIdentity(operation.Request().Binding.Supervisor, meta.SupervisorConfig) {
		return nil, task.ErrEvidenceFault
	}
	return operation.ReadProofContext(ctx)
}

// prepareMatchedProfileForRunner adds the final executable identity check that
// is possible only inside the queued runner process, immediately before the
// provider turn. Managed state-root runners must still match their content
// address; inspection workers retain their separately pinned identity.
func prepareMatchedProfileForRunner(deps Dependencies, root string, req task.TaskRecord, meta task.MetaRecord, store *taskdir.Store, currentExecutable, runnerOwnership string) (profile PreparedProfile, resultErr error) {
	candidate, facts, err := prepareExistingCandidate(deps, root, req, meta)
	if err != nil {
		return PreparedProfile{}, err
	}
	if err = validateRunnerExecutableBeforeProvider(root, candidate, req, meta, store, currentExecutable, runnerOwnership); err != nil {
		return PreparedProfile{}, err
	}
	return finalizeMatchedProfile(candidate, facts, root, req, meta)
}

func validateRunnerExecutableBeforeProvider(root string, candidate commonprovider.ProfileCandidate, req task.TaskRecord, meta task.MetaRecord, store *taskdir.Store, currentExecutable, runnerOwnership string) error {
	if candidate.Inspection == nil {
		return validateManagedOrdinaryRunnerExecutable(root, meta.RunnerExecutable, currentExecutable, runnerOwnership)
	}
	return validateInspectionRunnerExecutable(root, req, meta, store, currentExecutable)
}

func validateManagedOrdinaryRunnerExecutable(root, recorded, current, ownership string) error {
	if ownership != task.RunnerOwnershipManaged {
		return nil
	}
	if current == "" {
		var err error
		current, err = os.Executable()
		if err != nil {
			return task.ErrEvidenceFault
		}
	}
	return validateManagedTaskRunnerExecutable(root, recorded, current)
}

func validateInspectionRunnerExecutable(root string, req task.TaskRecord, meta task.MetaRecord, store *taskdir.Store, currentExecutable string) (resultErr error) {
	if store == nil {
		return task.ErrEvidenceFault
	}
	operation, err := inspection.LoadOperation(store, req.TaskID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if currentExecutable == "" {
		currentExecutable, err = os.Executable()
		if err != nil {
			return task.ErrEvidenceFault
		}
	}
	return validateProviderRunnerExecutable(root, operation.Request().Binding, meta.RunnerOwnership, currentExecutable)
}

func validateManagedTaskRunnerExecutable(root, recorded, current string) error {
	canonical, err := config.CanonicalizePath(current)
	if err != nil {
		return task.ErrEvidenceFault
	}
	if isStateRunnerPath(root, canonical) {
		if currentManagedRunnerExecutable(root, canonical) {
			return nil
		}
		return task.ErrEvidenceFault
	}
	recordedPath, err := config.CanonicalizePath(recorded)
	if err != nil || canonical != recordedPath {
		return task.ErrEvidenceFault
	}
	info, err := buildinfo.ReadFile(canonical)
	if err != nil || !managedRunnerBuildInfo(info) {
		return task.ErrEvidenceFault
	}
	return nil
}

func finalizeMatchedProfile(candidate commonprovider.ProfileCandidate, facts json.RawMessage, root string, req task.TaskRecord, meta task.MetaRecord) (PreparedProfile, error) {
	var err error
	facts, err = factsForCurrentRuntime(candidate, facts)
	if err != nil {
		return PreparedProfile{}, err
	}
	profile, err := finalizeCandidate(candidate, req, facts)
	if err != nil {
		return PreparedProfile{}, err
	}
	if err = matchPreparedProfile(candidate, profile, req, meta); err != nil {
		return PreparedProfile{}, err
	}
	if err = profile.ValidateStatePlacement(root); err != nil {
		return PreparedProfile{}, err
	}
	return profile, nil
}

func prepareExistingCandidate(deps Dependencies, root string, req task.TaskRecord, meta task.MetaRecord) (commonprovider.ProfileCandidate, json.RawMessage, error) {
	return prepareExistingCandidateContext(context.Background(), deps, root, req, meta)
}

func prepareExistingCandidateContext(ctx context.Context, deps Dependencies, root string, req task.TaskRecord, meta task.MetaRecord) (commonprovider.ProfileCandidate, json.RawMessage, error) {
	normalized := deps.normalized()
	var candidate commonprovider.ProfileCandidate
	var err error
	if !normalized.useCatalogExistingFor(req) {
		candidate, err = prepareCandidate(normalized, root, req)
	} else {
		candidate, err = normalized.Catalog.ExistingCandidate(req, meta)
		if err == nil {
			err = candidate.ValidateStatePlacement(root)
		}
	}
	if err != nil {
		return commonprovider.ProfileCandidate{}, nil, err
	}
	if candidate.Inspection == nil {
		return candidate, nil, nil
	}
	facts, err := storedInspectionFactsContext(ctx, deps, root, req, meta, candidate)
	return candidate, facts, err
}

func prepareInspectionWorkerCandidate(deps Dependencies, root string, req task.TaskRecord, meta *task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	normalized := deps.normalized()
	if meta == nil || !normalized.useCatalogExistingFor(req) {
		return prepareCandidate(normalized, root, req)
	}
	candidate, err := normalized.Catalog.ExistingCandidate(req, *meta)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if err = candidate.ValidateStatePlacement(root); err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	return candidate, nil
}

func (d Dependencies) useCatalogExistingFor(request task.TaskRecord) bool {
	if d.useCatalogExisting {
		return true
	}
	if d.PrepareProfile != nil {
		return false
	}
	registration, err := d.Catalog.Lookup(request.Provider)
	return err == nil && registration.PrepareExisting != nil
}

func runtimeOnlyInspectionDefinition(definition commonprovider.InspectionDefinition) bool {
	return definition.Runtime != nil && definition.Models == nil && len(definition.Arguments) == 0 && definition.Project == nil && definition.Remote == nil
}

func runtimeOwnsInspectionExecutable(definition commonprovider.InspectionDefinition) bool {
	return definition.Runtime != nil && definition.Executable == definition.Runtime.Executable && definition.ExecutableSHA256 == definition.Runtime.ExecutableSHA256
}

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

func freshPreflightProfile(deps Dependencies, root string, req task.TaskRecord, meta task.MetaRecord, scope execution.PreflightScope) (execution.Plan, error) {
	candidate, facts, err := prepareExistingCandidateContext(scope.Context(), deps, root, req, meta)
	if err != nil {
		return execution.Plan{}, err
	}
	if candidate.Inspection != nil {
		freshFacts, inspectErr := inspection.Inspect(scope, *candidate.Inspection)
		if inspectErr != nil {
			return execution.Plan{}, inspectErr
		}
		// Runtime-only journals from before capability digests were recorded do
		// not contain enough information to compare command shape. The current
		// supervised probe is therefore authoritative for those records. Native
		// projections remain byte-for-byte bound to their admission proof.
		if !runtimeOnlyInspectionDefinition(*candidate.Inspection) && !inspectionFactsMatchForDefinition(facts, freshFacts, candidate.Inspection.Runtime != nil) {
			return execution.Plan{}, task.ErrEvidenceFault
		}
		facts = freshFacts
	}
	profile, err := finalizeCandidate(candidate, req, facts)
	if err != nil {
		return execution.Plan{}, err
	}
	if err = matchPreparedProfile(candidate, profile, req, meta); err != nil {
		return execution.Plan{}, err
	}
	if err = profile.ValidateStatePlacement(root); err != nil {
		return execution.Plan{}, err
	}
	return profile.Plan, nil
}

func matchPreparedProfile(candidate commonprovider.ProfileCandidate, profile PreparedProfile, req task.TaskRecord, meta task.MetaRecord) error {
	if candidate.Inspection != nil && candidate.Inspection.Runtime != nil {
		return profile.MatchesWithRuntimeRefresh(req, meta)
	}
	return profile.Matches(req, meta)
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
