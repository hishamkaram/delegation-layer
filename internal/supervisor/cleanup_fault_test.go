package supervisor

import (
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

func openHelperControl(t *testing.T, fixture *helperAdmissionFixture, store *taskdir.Store) *taskdir.ControlDir {
	t.Helper()
	control, err := store.OpenInspectionContext(t.Context(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true)
	if control != nil {
		fixture.controls = append(fixture.controls, control)
	}
	if err != nil {
		t.Fatal(err)
	}
	return control
}

type helperMaintenanceCleanupFault struct {
	taskdir.BaseFaultInjector
	enabled  atomic.Bool
	attempts atomic.Int32
	cause    error
}

func (*helperMaintenanceCleanupFault) OnLockFileBarrier(string) error      { return nil }
func (*helperMaintenanceCleanupFault) OnLockParentDirBarrier(string) error { return nil }
func (f *helperMaintenanceCleanupFault) OnFlock(path string, operation int) error {
	if f.enabled.Load() && filepath.Base(path) == ".maintenance.lock" && operation == unix.LOCK_UN {
		f.attempts.Add(1)
		return f.cause
	}
	return nil
}
