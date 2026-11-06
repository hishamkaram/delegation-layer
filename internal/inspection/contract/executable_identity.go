package contract

import (
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func validateBindingExecutableIdentity(binding Binding) error {
	if err := binding.ExecutableIdentity.Validate(); err != nil {
		return fmt.Errorf("inspection executable identity: %w", err)
	}
	if binding.ExecutableIdentity == task.ExecutableIdentityInstalledPathV1 && (binding.HelperSHA256 != "" || binding.WorkerSHA256 != "") {
		return errors.New("installed inspection identity contains executable digests")
	}
	return nil
}

func validateBindingExecutables(binding Binding) error {
	if err := validateBindingExecutableIdentity(binding); err != nil {
		return err
	}
	for name, value := range map[string]string{"inspection helper executable": binding.HelperExecutable, "inspection worker executable": binding.WorkerExecutable} {
		if err := validateAbsoluteCleanPath(name, value); err != nil {
			return err
		}
	}
	if binding.ExecutableIdentity == task.ExecutableIdentityInstalledPathV1 {
		return nil
	}
	if binding.HelperSHA256 != "" {
		if err := task.ValidateSHA256(binding.HelperSHA256); err != nil {
			return fmt.Errorf("inspection helper digest: %w", err)
		}
	}
	if err := task.ValidateSHA256(binding.WorkerSHA256); err != nil {
		return fmt.Errorf("inspection worker digest: %w", err)
	}
	return nil
}
