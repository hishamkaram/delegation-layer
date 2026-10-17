package admission

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func (profiles Profiles) Existing(root string, req task.TaskRecord, meta task.MetaRecord) (commonprovider.ProfileCandidate, json.RawMessage, error) {
	return profiles.ExistingContext(context.Background(), root, req, meta)
}

func (profiles Profiles) ExistingContext(ctx context.Context, root string, req task.TaskRecord, meta task.MetaRecord) (commonprovider.ProfileCandidate, json.RawMessage, error) {
	candidate, err := profiles.candidateForTask(root, req, &meta)
	if err != nil {
		return commonprovider.ProfileCandidate{}, nil, err
	}
	if meta.RuntimePreflightSHA256 != "" {
		if err = ValidatePendingRuntimeCandidate(candidate, meta); err != nil {
			return commonprovider.ProfileCandidate{}, nil, err
		}
		return candidate, nil, nil
	}
	if candidate.Inspection == nil {
		return candidate, nil, nil
	}
	facts, err := profiles.storedFactsContext(ctx, root, req, meta, candidate)
	return candidate, facts, err
}

func (profiles Profiles) storedFactsContext(ctx context.Context, root string, req task.TaskRecord, meta task.MetaRecord, candidate commonprovider.ProfileCandidate) (facts json.RawMessage, resultErr error) {
	store, err := profiles.openStore(ctx, root)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	operation, err := LoadInspectionOperationContext(ctx, store, req)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if err = ValidateInspectionCandidate(ctx, operation, req, candidate); err != nil {
		return nil, err
	}
	if !task.SameSupervisorIdentity(operation.Request().Binding.Supervisor, meta.SupervisorConfig) {
		return nil, task.ErrEvidenceFault
	}
	return operation.ReadProofContext(ctx)
}

func (profiles Profiles) Fresh(root string, req task.TaskRecord, meta task.MetaRecord, scope execution.PreflightScope) (execution.Plan, error) {
	candidate, facts, err := profiles.ExistingContext(scope.Context(), root, req, meta)
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
	profile, err := FinalizeCandidate(candidate, req, facts)
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
