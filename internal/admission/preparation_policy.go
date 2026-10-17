package admission

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func ProfileForAdmission(candidate commonprovider.ProfileCandidate, req task.TaskRecord, deferRuntime bool) (commonprovider.PreparedProfile, error) {
	if deferRuntime {
		return AdmitCandidate(candidate, req)
	}
	return initialAdmissionProfile(candidate, req)
}

func initialAdmissionProfile(candidate commonprovider.ProfileCandidate, req task.TaskRecord) (commonprovider.PreparedProfile, error) {
	if candidate.Inspection != nil {
		return commonprovider.PreparedProfile{}, nil
	}
	return FinalizeCandidate(candidate, req, nil)
}

func SupervisorOptionsForCandidate(base pueue.Options, candidate commonprovider.ProfileCandidate) (pueue.Options, error) {
	if base.ObservationTimeout < 0 {
		return pueue.Options{}, pueue.ErrConfiguration
	}
	if candidate.Inspection == nil {
		return base, nil
	}
	definition, _, err := candidate.Inspection.Snapshot()
	if err != nil {
		return pueue.Options{}, err
	}
	return supervisorcore.OptionsWithEnvironment(base, definition.Environment), nil
}
