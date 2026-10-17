package supervisor

import (
	"errors"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/pueue"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func ApplyContinuationEnvironment(root string, meta *task.MetaRecord, apply func([]string, bool) (func() error, error)) (func() error, error) {
	if apply == nil {
		return nil, task.ErrEvidenceFault
	}
	restore, err := apply(meta.Environment, meta.EnvironmentRecorded)
	if err != nil {
		return nil, err
	}
	if !pueue.IsPrivateConfig(root, meta.SupervisorConfig.ConfigPath) {
		return restore, nil
	}
	if err = os.Unsetenv("DELEGATE_PUEUE_CONFIG"); err != nil {
		return nil, errors.Join(err, restore())
	}
	return restore, nil
}
