//go:build linux

package taskdir

import (
	"fmt"
	"os"
)

func platformBarrierFile(f *os.File) error {
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing file %s: %w", f.Name(), err)
	}
	return nil
}
