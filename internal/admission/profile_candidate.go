package admission

import (
	"slices"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func (profiles Profiles) Prepare(root string, req task.TaskRecord) (commonprovider.ProfileCandidate, error) {
	candidate, err := profiles.candidates.Current(req)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	return validatePreparedCandidate(root, req, candidate)
}

func (profiles Profiles) Refresh(root string, req task.TaskRecord, admitted commonprovider.ProfileCandidate) (commonprovider.ProfileCandidate, error) {
	if admitted.Inspection == nil || admitted.Inspection.Runtime == nil {
		return admitted, nil
	}
	current, err := profiles.Prepare(root, req)
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

func (profiles Profiles) ForWorker(root string, req task.TaskRecord, meta *task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	return profiles.candidateForTask(root, req, meta)
}

func (profiles Profiles) candidateForTask(root string, req task.TaskRecord, meta *task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	candidate, historical, err := profiles.candidates.ForTask(req, meta)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	if !historical {
		return validatePreparedCandidate(root, req, candidate)
	}
	if err = candidate.ValidateStatePlacement(root); err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	return candidate, nil
}

func validatePreparedCandidate(root string, req task.TaskRecord, candidate commonprovider.ProfileCandidate) (commonprovider.ProfileCandidate, error) {
	if candidate.Directory != req.CanonicalCwd || candidate.Finalize == nil {
		return commonprovider.ProfileCandidate{}, commonprovider.ErrProfileUnavailable
	}
	if err := candidate.ValidateStatePlacement(root); err != nil {
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
