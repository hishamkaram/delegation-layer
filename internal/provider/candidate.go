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

// PrepareExistingCandidate reconstructs a previously admitted profile when a
// provider has to retain a historical preparation contract across a release.
// New admission always uses PrepareCandidate.
type PrepareExistingCandidate func(task.TaskRecord, task.MetaRecord) (ProfileCandidate, error)

// ProfileCandidate separates a compiled policy description from its external
// inspection. Finalize is pure; it accepts only validated nonsecret facts.
// No process, storage, or supervisor authority crosses this boundary.
type ProfileCandidate struct {
	Directory     string
	WritableRoots []string
	Inspection    *InspectionDefinition
	// Admit prepares a static runtime-only contract. Its pending capability
	// digest requires this task's supervised preflight before execution.
	// Identity and interaction handlers must derive solely from the immutable
	// request and predicate, never from runtime observations.
	Admit    func() (PreparedProfile, error)
	Finalize func(facts json.RawMessage, now time.Time) (PreparedProfile, error)
}

// AdmissionProfile is the single boundary for a runtime-only static profile.
// It binds the launched executable and required capability to this candidate
// without inventing inspected facts or an observed version.
func (c ProfileCandidate) AdmissionProfile(request task.TaskRecord) (PreparedProfile, error) {
	if c.Admit == nil || c.Inspection == nil || !c.Inspection.RuntimeOnly() {
		return PreparedProfile{}, ErrProfileUnavailable
	}
	profile, err := c.Admit()
	if err != nil {
		return PreparedProfile{}, err
	}
	capability, err := RuntimeCapabilityDigest(*c.Inspection)
	if err != nil || profile.RuntimePreflightSHA256 != capability || profile.Plan.Directory != c.Directory ||
		!slices.Equal(profile.WritableRoots, c.WritableRoots) {
		return PreparedProfile{}, task.ErrIdentityMismatch
	}
	if err = c.ValidateRuntimeProfile(profile); err != nil {
		return PreparedProfile{}, err
	}
	profile.Plan.RuntimePreflightSHA256 = capability
	if err = profile.ValidateAdmission(request); err != nil {
		return PreparedProfile{}, err
	}
	return cloneCandidateProfile(profile), nil
}

// ValidateRuntimeProfile binds the execution plan to the executable whose
// runtime-only capabilities are inspected. Native projections retain their
// separate helper identity and are not subject to this equality.
func (c ProfileCandidate) ValidateRuntimeProfile(profile PreparedProfile) error {
	if c.Inspection == nil || !c.Inspection.RuntimeOnly() {
		return nil
	}
	if profile.Plan.Executable != c.Inspection.Runtime.Executable || profile.Plan.ExecutableSHA256 != c.Inspection.Runtime.ExecutableSHA256 {
		return task.ErrIdentityMismatch
	}
	return nil
}

// InspectionDefinition describes an optional native projection and/or a
// supervised runtime capability probe. Core owns stdin, pipes, Start/Wait,
// deadlines, projection containment, and publication.
// Project must return canonical nonsecret JSON, never native credential bytes.
// Core discards error/panic values and publishes only fixed failure codes.
type InspectionDefinition struct {
	Models           *ModelDiscoveryDefinition                    `json:"models,omitempty"`
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

// RuntimeOnly reports whether the inspection carries only CLI capabilities,
// with no native projection that determines the admission policy.
func (d InspectionDefinition) RuntimeOnly() bool {
	return d.Runtime != nil && d.Models == nil && len(d.Arguments) == 0 && d.Project == nil && d.Remote == nil
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
