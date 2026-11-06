// Package verifiedexec owns metadata lookup and disposal of historical executable pools.
package verifiedexec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/config"
)

// LocatePath resolves an installed role and validates its native file metadata.
// It never opens or reads executable contents.
func LocatePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve installed executable: %w", err)
	}
	canonical, err := config.CanonicalizePath(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve installed executable: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("inspect installed executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("installed executable is not a regular executable")
	}
	return canonical, nil
}
