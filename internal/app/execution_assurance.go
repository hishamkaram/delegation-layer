package app

import "github.com/hishamkaram/delegation-layer/internal/task"

const executionAssuranceFeature = "task-lifetime-assurance-v1"

// ExecutionAssuranceState distinguishes a recorded runtime backend from a
// task with no published lifetime registration. It is not execution closure.
type ExecutionAssuranceState string

const (
	ExecutionAssuranceRegistered   ExecutionAssuranceState = "registered"
	ExecutionAssuranceUnregistered ExecutionAssuranceState = "unregistered"
)

// ExecutionBackend identifies the backend selected by the task's guardian.
type ExecutionBackend string

const (
	ExecutionBackendCgroupV2     ExecutionBackend = "cgroup-v2"
	ExecutionBackendProcessGroup ExecutionBackend = "process-group"
)

// ExecutionAssuranceLevel names that recorded backend's containment guarantee.
type ExecutionAssuranceLevel string

const (
	ExecutionAssuranceExact       ExecutionAssuranceLevel = "exact"
	ExecutionAssuranceCooperative ExecutionAssuranceLevel = "cooperative"
)

// ExecutionAssuranceObservation projects the actual immutable registration.
// Guardian owns that record; this diagnostic neither selects a backend nor
// infers process termination from it. Native scope identifiers stay private.
type ExecutionAssuranceObservation struct {
	Feature            string                  `json:"feature"`
	State              ExecutionAssuranceState `json:"state"`
	Backend            ExecutionBackend        `json:"backend,omitempty"`
	Assurance          ExecutionAssuranceLevel `json:"assurance,omitempty"`
	RegistrationSHA256 string                  `json:"registration_sha256,omitempty"`
}

func optionalExecutionAssurance(include bool, evidence task.LifetimeEvidence) (*ExecutionAssuranceObservation, error) {
	if !include {
		return nil, nil
	}
	return describeExecutionAssurance(evidence)
}

func describeExecutionAssurance(evidence task.LifetimeEvidence) (*ExecutionAssuranceObservation, error) {
	if _, err := task.ResolveLifetimeClosure(evidence); err != nil {
		return nil, err
	}
	observation := &ExecutionAssuranceObservation{Feature: executionAssuranceFeature, State: ExecutionAssuranceUnregistered}
	registration := evidence.Registration
	if registration == nil {
		return observation, nil
	}
	switch registration.ScopeKind {
	case task.LifetimeScopeCgroupV2:
		observation.Backend, observation.Assurance = ExecutionBackendCgroupV2, ExecutionAssuranceExact
	case task.LifetimeScopeProcessGroup:
		observation.Backend, observation.Assurance = ExecutionBackendProcessGroup, ExecutionAssuranceCooperative
	default:
		return nil, task.ErrIdentityMismatch
	}
	data, err := task.MarshalCanonical(registration)
	if err != nil {
		return nil, err
	}
	observation.State = ExecutionAssuranceRegistered
	observation.RegistrationSHA256 = task.ComputeSHA256(data)
	return observation, nil
}
