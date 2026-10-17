package admission

import (
	"encoding/json"
	"slices"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func FinalizeCandidate(candidate commonprovider.ProfileCandidate, req task.TaskRecord, facts json.RawMessage) (commonprovider.PreparedProfile, error) {
	if candidate.Finalize == nil || (candidate.Inspection != nil && len(facts) == 0) {
		return commonprovider.PreparedProfile{}, commonprovider.ErrProfileUnavailable
	}
	profile, err := candidate.Finalize(facts, time.Now())
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	if err = profile.Validate(req); err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	if err = candidate.ValidateRuntimeProfile(profile); err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	if profile.Plan.Directory != candidate.Directory || !slices.Equal(profile.WritableRoots, candidate.WritableRoots) {
		return commonprovider.PreparedProfile{}, commonprovider.ErrProfileUnavailable
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

func AdmitCandidate(candidate commonprovider.ProfileCandidate, req task.TaskRecord) (commonprovider.PreparedProfile, error) {
	return candidate.AdmissionProfile(req)
}

func finalizeMatchedProfile(candidate commonprovider.ProfileCandidate, facts json.RawMessage, root string, req task.TaskRecord, meta task.MetaRecord) (commonprovider.PreparedProfile, error) {
	if meta.RuntimePreflightSHA256 != "" {
		profile, err := AdmitCandidate(candidate, req)
		if err != nil {
			return commonprovider.PreparedProfile{}, err
		}
		if err = profile.MatchesPendingRuntimeRefresh(req, meta); err != nil {
			return commonprovider.PreparedProfile{}, err
		}
		if err = profile.ValidateStatePlacement(root); err != nil {
			return commonprovider.PreparedProfile{}, err
		}
		return profile, nil
	}
	var err error
	facts, err = factsForCurrentRuntime(candidate, facts)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	profile, err := FinalizeCandidate(candidate, req, facts)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	if err = matchPreparedProfile(candidate, profile, req, meta); err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	if err = profile.ValidateStatePlacement(root); err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	return profile, nil
}

// prepareMatchedProfile refreshes the provider's compiled launch profile and
// checks it against the immutable admission record. Callers use it immediately
// before any fresh submission and again after supervisor reconciliation, so a
// policy or placement change cannot cross the next authority boundary.
func (profiles Profiles) Match(root string, req task.TaskRecord, meta task.MetaRecord) (commonprovider.PreparedProfile, error) {
	candidate, facts, err := profiles.Existing(root, req, meta)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	return finalizeMatchedProfile(candidate, facts, root, req, meta)
}
