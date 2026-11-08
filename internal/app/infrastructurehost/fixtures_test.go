package infrastructurehost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/infrastructure"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type testStores struct{}

func (testStores) OpenNative(context.Context, string) (*taskdir.Store, error) {
	return nil, os.ErrInvalid
}

func (testStores) RetainsStore(cause error, store *taskdir.Store) bool {
	return taskdir.CleanupRetainsStore(cause, store)
}

func canonicalTestExecutable(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return supervisorfixture.CanonicalPath(t, executable)
}

func testCallbacks() Callbacks {
	return Callbacks{
		MaintenanceLaunch: func(context.Context, string, string, []string) error { return nil },
		SuccessfulCleanup: func(context.Context, string, task.SupervisorRef, []pueue.Job) (pueue.PrivateQueueCleanupPlan, error) {
			return pueue.PrivateQueueCleanupPlan{}, nil
		},
		TerminalEvidence:       func(context.Context, string, task.SupervisorRef, []pueue.Job) error { return nil },
		TerminalCustody:        func(context.Context, string, task.SupervisorRef, pueue.PrivateTerminalCustodyScope) error { return nil },
		MigrationAuthorization: func(context.Context, string, task.SupervisorRef, []pueue.Job) error { return nil },
	}
}

func writeFixtureFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func setAppInvocationOrigin(t *testing.T, environment []string) {
	t.Helper()
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if found && key == infrastructure.OriginEnvironment {
			t.Setenv(key, value)
			return
		}
	}
	t.Fatal("fixture has no origin")
}

func unsetAppInvocationOrigin(t *testing.T) {
	t.Helper()
	old, present := os.LookupEnv(infrastructure.OriginEnvironment)
	t.Cleanup(func() {
		var err error
		if present {
			err = os.Setenv(infrastructure.OriginEnvironment, old)
		} else {
			err = os.Unsetenv(infrastructure.OriginEnvironment)
		}
		if err != nil {
			t.Error(err)
		}
	})
	if err := os.Unsetenv(infrastructure.OriginEnvironment); err != nil {
		t.Fatal(err)
	}
}

func cleanupLifetimeLease(t *testing.T, store *taskdir.Store) *taskdir.LifetimeOwnerLease {
	t.Helper()
	control, err := store.OpenInfrastructureContext(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := control.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		store.SetFaultInjector(nil)
		if err := errors.Join(lease.Release(), control.Close()); err != nil {
			t.Error(err)
		}
	})
	return lease
}

type preparationUnlockFault struct {
	taskdir.BaseFaultInjector
	cause error
}

func (*preparationUnlockFault) OnLockFileBarrier(string) error      { return nil }
func (*preparationUnlockFault) OnLockParentDirBarrier(string) error { return nil }
func (f *preparationUnlockFault) OnFlock(path string, operation int) error {
	if filepath.Base(path) == ".lifetime-owner.lock" && operation&unix.LOCK_UN != 0 {
		return f.cause
	}
	return nil
}
