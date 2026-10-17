package contract

import (
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func ValidateAbsoluteCleanPath(path string) error {
	if path == "" || strings.ContainsRune(path, 0) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return task.ErrIdentityMismatch
	}
	return nil
}
