package contract

import (
	"encoding/json"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// bindingJSON is a derived wire view. Pointers distinguish historical required
// empty SHA fields from omitted installed-path fields without changing order.
type bindingJSON struct {
	DefinitionRevision  string                  `json:"definition_revision"`
	DefinitionSHA256    string                  `json:"definition_sha256"`
	CapabilitySHA256    string                  `json:"capability_sha256,omitempty"`
	HelperExecutable    string                  `json:"helper_executable"`
	HelperSHA256        *string                 `json:"helper_sha256,omitempty"`
	WorkerExecutable    string                  `json:"worker_executable"`
	WorkerSHA256        *string                 `json:"worker_sha256,omitempty"`
	ExecutableIdentity  task.ExecutableIdentity `json:"executable_identity,omitempty"`
	RunnerOwnership     string                  `json:"runner_ownership,omitempty"`
	WorkerExitProof     string                  `json:"worker_exit_proof,omitempty"`
	Environment         []string                `json:"environment,omitempty"`
	EnvironmentRecorded bool                    `json:"environment_recorded,omitempty"`
	Supervisor          task.SupervisorRef      `json:"supervisor"`
}

// MarshalJSON preserves legacy encoding, including the required empty helper
// SHA field, while the explicit installed form omits both executable digests.
func (binding Binding) MarshalJSON() ([]byte, error) {
	if err := validateBindingExecutableIdentity(binding); err != nil {
		return nil, err
	}
	helperSHA, workerSHA := &binding.HelperSHA256, &binding.WorkerSHA256
	if binding.ExecutableIdentity == task.ExecutableIdentityInstalledPathV1 {
		helperSHA, workerSHA = nil, nil
	}
	return json.Marshal(bindingJSON{
		DefinitionRevision: binding.DefinitionRevision, DefinitionSHA256: binding.DefinitionSHA256, CapabilitySHA256: binding.CapabilitySHA256,
		HelperExecutable: binding.HelperExecutable, HelperSHA256: helperSHA,
		WorkerExecutable: binding.WorkerExecutable, WorkerSHA256: workerSHA, ExecutableIdentity: binding.ExecutableIdentity,
		RunnerOwnership: binding.RunnerOwnership, WorkerExitProof: binding.WorkerExitProof,
		Environment: binding.Environment, EnvironmentRecorded: binding.EnvironmentRecorded, Supervisor: binding.Supervisor,
	})
}

func (binding *Binding) UnmarshalJSON(data []byte) error {
	type plain Binding
	var decoded plain
	if err := task.DecodeStrict(data, &decoded); err != nil {
		return err
	}
	if err := validateBindingExecutableIdentity(Binding(decoded)); err != nil {
		return err
	}
	if decoded.ExecutableIdentity == "" {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		for _, name := range []string{"helper_sha256", "worker_sha256"} {
			if _, present := fields[name]; !present {
				return errors.New("legacy inspection binding requires executable digest fields")
			}
		}
	}
	*binding = Binding(decoded)
	return nil
}
