package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func ContinuationRunner(root, savedRunner, ownership string) (string, error) {
	if ownership == task.RunnerOwnershipCustom {
		if savedRunner == "" {
			return "", task.ErrEvidenceFault
		}
		if _, err := os.Stat(savedRunner); err != nil {
			return "", fmt.Errorf("predecessor custom runner %q is unavailable: %w", savedRunner, err)
		}
		return savedRunner, nil
	}
	if savedRunner == "" || IsStateRunnerPath(root, filepath.Clean(savedRunner)) {
		return "", nil
	}
	if _, err := os.Stat(savedRunner); err == nil || !errors.Is(err, os.ErrNotExist) {
		return savedRunner, nil
	}
	return "", nil
}
