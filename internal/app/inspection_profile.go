package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

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
	return profile, nil
}

// validateInspectionCandidate binds the reconstructed compiled definition and
// full immutable task to existing evidence. It never reads native credentials.
func validateInspectionCandidate(operation *inspection.Operation, req task.TaskRecord, candidate commonprovider.ProfileCandidate) error {
	if operation == nil || candidate.Inspection == nil {
		return task.ErrEvidenceFault
	}
	definition, digest, err := candidate.Inspection.Snapshot()
	if err != nil {
		return task.ErrEvidenceFault
	}
	record := operation.Request()
	requestBytes, err := task.MarshalCanonical(req)
	if err != nil {
		return task.ErrEvidenceFault
	}
	if record.TaskSHA256 != task.ComputeSHA256(requestBytes) || record.Binding.DefinitionRevision != definition.Revision || record.Binding.DefinitionSHA256 != digest || record.Binding.HelperExecutable != definition.Executable || record.Binding.HelperSHA256 != definition.ExecutableSHA256 {
		return task.ErrEvidenceFault
	}
	workerDigest, err := commonprovider.FingerprintExecutable(record.Binding.WorkerExecutable)
	if err != nil || workerDigest != record.Binding.WorkerSHA256 {
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
	if err = validateInspectionCandidate(operation, req, candidate); err != nil {
		return nil, err
	}
	if operation.Request().Binding.Supervisor != meta.SupervisorConfig {
		return nil, task.ErrEvidenceFault
	}
	return operation.ReadProofContext(ctx)
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

func freshPreflightProfile(deps Dependencies, root string, req task.TaskRecord, meta task.MetaRecord, scope execution.PreflightScope) error {
	candidate, facts, err := prepareExistingCandidateContext(scope.Context(), deps, root, req, meta)
	if err != nil {
		return err
	}
	if candidate.Inspection != nil {
		freshFacts, inspectErr := inspection.Inspect(scope, *candidate.Inspection)
		if inspectErr != nil {
			return inspectErr
		}
		if !inspectionFactsMatch(facts, freshFacts) {
			return task.ErrEvidenceFault
		}
		facts = freshFacts
	}
	profile, err := finalizeCandidate(candidate, req, facts)
	if err != nil {
		return err
	}
	if err = profile.Matches(req, meta); err != nil {
		return err
	}
	return profile.ValidateStatePlacement(root)
}

// inspectionFactsMatch compares the canonical nonsecret projection returned
// by admission with the fresh projection at the ordinary start boundary. Both
// inspection APIs already canonicalize their object, while this normalization
// keeps the comparison independent of object field order at this caller.
func inspectionFactsMatch(stored, fresh json.RawMessage) bool {
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
