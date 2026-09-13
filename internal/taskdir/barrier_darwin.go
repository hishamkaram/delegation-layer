//go:build darwin

package taskdir

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func platformBarrierFile(f *os.File) error {
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing file %s: %w", f.Name(), err)
	}
	if _, err := unix.FcntlInt(f.Fd(), unix.F_FULLFSYNC, 0); err != nil {
		return fmt.Errorf("full fsync file %s: %w", f.Name(), err)
	}
	return nil
}
