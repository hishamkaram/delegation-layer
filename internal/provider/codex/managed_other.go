//go:build !darwin

package codex

import (
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func managedSources() ([]task.PolicySourceDigest, error) {
	return nil, fmt.Errorf("%w: managed policy inspection is not certified on this platform", ErrUnsupportedProfile)
}
