package admission

import (
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func finalizeInspection(selection supervisorcore.AdmissionSelection, options PreparationOptions, store *taskdir.Store, req task.TaskRecord, candidate commonprovider.ProfileCandidate, supervisor *pueue.Client) (commonprovider.PreparedProfile, commonprovider.ProfileCandidate, error) {
	var err error
	candidate, err = options.Profiles.Refresh(store.Root, req, candidate)
	if err != nil {
		return commonprovider.PreparedProfile{}, commonprovider.ProfileCandidate{}, WithStage("finalize-provider", err)
	}
	facts, _, err := InspectProvider(InspectionOptions{Runner: selection.Runner, DefaultRunner: options.Supervisor.RunnerExecutable, RunnerOwnership: supervisorcore.RequestedRunnerOwnership(selection), PreserveGeneration: selection.SavedSupervisor != nil}, store, req, candidate, supervisor, false)
	if err != nil {
		return commonprovider.PreparedProfile{}, commonprovider.ProfileCandidate{}, WithStage("inspect-provider", err)
	}
	// The inspection worker reconstructs the candidate when it actually runs.
	// Refresh once more after its proof is complete so finalization uses the
	// same current provider identity that the worker inspected. The finalizer
	// still binds the returned facts to this candidate; a replacement during
	// this last handoff fails closed instead of relabeling the proof.
	candidate, err = options.Profiles.Refresh(store.Root, req, candidate)
	if err != nil {
		return commonprovider.PreparedProfile{}, commonprovider.ProfileCandidate{}, WithStage("finalize-provider", err)
	}
	profile, err := FinalizeCandidate(candidate, req, facts)
	if err != nil {
		return commonprovider.PreparedProfile{}, commonprovider.ProfileCandidate{}, WithStage("finalize-provider", err)
	}
	// The operation deadline bounds provider work and its evidence. Once that
	// evidence is timely and the provider identity is refreshed above, local
	// task creation must not race the already-expired provider clock.
	return profile, candidate, nil
}
