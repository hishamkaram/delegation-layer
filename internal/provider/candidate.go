package provider

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// PrepareCandidate performs only static preparation. The app resolves any
// requested native inspection before finalizing and admitting an ordinary task.
type PrepareCandidate func(task.TaskRecord) (ProfileCandidate, error)

// ProfileCandidate separates a compiled policy description from its external
// inspection. Finalize is pure; it accepts only validated nonsecret facts.
// No process, storage, or supervisor authority crosses this boundary.
type ProfileCandidate struct {
	Directory     string
	WritableRoots []string
	Inspection    *InspectionDefinition
	Finalize      func(facts json.RawMessage, now time.Time) (PreparedProfile, error)
}

// InspectionDefinition describes an optional native projection and/or a
// supervised runtime capability probe. Core owns stdin, pipes, Start/Wait,
// deadlines, projection containment, and publication.
// Project must return canonical nonsecret JSON, never native credential bytes.
// Core discards error/panic values and publishes only fixed failure codes.
type InspectionDefinition struct {
	Revision         string                                       `json:"revision"`
	Executable       string                                       `json:"executable"`
	ExecutableSHA256 string                                       `json:"executable_sha256"`
	Arguments        []string                                     `json:"arguments"`
	Directory        string                                       `json:"directory"`
	Environment      []string                                     `json:"environment"`
	OutputLimit      int64                                        `json:"output_limit"`
	Runtime          *RuntimeProbeDefinition                      `json:"runtime,omitempty"`
	Project          func(stdout []byte) (json.RawMessage, error) `json:"-"`
	Remote           *HTTPInspectionDefinition                    `json:"remote,omitempty"`
}

// HTTPInspectionDefinition describes at most one core-owned HTTPS GET after
// the native read. Authorization and Project are pure and handle transient
// sensitive bytes: their errors and panics must never cross the core boundary.
// Authorization returns required=false when the native facts need no fetch;
// Project then receives status zero and no body. Headers are fixed nonsecret
// protocol fields. Credentials belong only in the transient Authorization value.
type HTTPInspectionDefinition struct {
	URL           string                                                                `json:"url"`
	Headers       map[string]string                                                     `json:"headers"`
	Authorization func(native []byte) (value string, required bool, err error)          `json:"-"`
	Project       func(native []byte, status int, body []byte) (json.RawMessage, error) `json:"-"`
}

// ValidateStatePlacement reuses the admitted-profile overlap rules before an
// inspection exists. Static validation cannot depend on native credential data.
func (c ProfileCandidate) ValidateStatePlacement(root string) error {
	return (PreparedProfile{
		Plan:          execution.Plan{Directory: c.Directory},
		WritableRoots: c.WritableRoots,
	}).ValidateStatePlacement(root)
}

// ReadyCandidate adapts an existing finite profile factory to the two-stage
// contract. It cannot consume inspection facts or acquire process authority.
func ReadyCandidate(prepare PrepareProfile) PrepareCandidate {
	return func(request task.TaskRecord) (ProfileCandidate, error) {
		if prepare == nil {
			return ProfileCandidate{}, ErrProfileUnavailable
		}
		profile, err := prepare(request)
		if err != nil {
			return ProfileCandidate{}, err
		}
		if err = profile.Validate(request); err != nil {
			return ProfileCandidate{}, err
		}
		profile = cloneCandidateProfile(profile)
		return ProfileCandidate{
			Directory:     profile.Plan.Directory,
			WritableRoots: slices.Clone(profile.WritableRoots),
			Finalize: func(facts json.RawMessage, _ time.Time) (PreparedProfile, error) {
				if len(facts) != 0 {
					return PreparedProfile{}, fmt.Errorf("%w: ready profile received unexpected inspection facts", ErrProfileUnavailable)
				}
				return cloneCandidateProfile(profile), nil
			},
		}, nil
	}
}

// cloneCandidateProfile isolates factory storage and each finalization result.
func cloneCandidateProfile(profile PreparedProfile) PreparedProfile {
	profile.Plan.Arguments = slices.Clone(profile.Plan.Arguments)
	profile.Plan.Environment = slices.Clone(profile.Plan.Environment)
	profile.Plan.InputFiles = slices.Clone(profile.Plan.InputFiles)
	profile.Plan.OutputArtifacts = slices.Clone(profile.Plan.OutputArtifacts)
	profile.WritableRoots = slices.Clone(profile.WritableRoots)
	profile.Effective = task.CloneEffectiveConfig(profile.Effective)
	return profile
}
