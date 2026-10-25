package admission

import (
	"fmt"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Profiles carries immutable factory and record-reader configuration plus the
// launch environment in scope for provider preparation. Profiles is the only
// owner of that scope: a recorded selection returns a new value and never
// changes the caller's. Task and inspection journals remain the sole owners of
// durable admission evidence.
type Profiles struct {
	candidates  CandidateSource
	openStore   ProfileStoreOpener
	environment []string
}

// NewProfiles binds a captured launch environment. Nil means the composition
// did not capture one, and every preparation then fails instead of reading
// the process environment; a nonnil empty slice is an explicit selection.
func NewProfiles(candidates CandidateSource, openStore ProfileStoreOpener, environment []string) Profiles {
	return Profiles{candidates: candidates, openStore: openStore, environment: slices.Clone(environment)}
}

// Environment returns a copy of the launch environment in scope.
func (profiles Profiles) Environment() []string {
	return slices.Clone(profiles.environment)
}

// ForRecord selects a saved launch scope over this one with the same rule the
// former process-level swap used: an unrecorded empty scope keeps the current
// environment, otherwise the validated saved values replace it.
func (profiles Profiles) ForRecord(values []string, recorded bool) (Profiles, error) {
	base, err := profiles.scopeEnvironment()
	if err != nil {
		return Profiles{}, err
	}
	selected, err := task.SelectEnvironment(base, values, recorded)
	if err != nil {
		return Profiles{}, err
	}
	profiles.environment = selected
	return profiles, nil
}

// scopeEnvironment returns an independent copy for one factory call.
func (profiles Profiles) scopeEnvironment() ([]string, error) {
	if profiles.environment == nil {
		return nil, fmt.Errorf("%w: profile launch environment was not captured", task.ErrEvidenceFault)
	}
	return slices.Clone(profiles.environment), nil
}
