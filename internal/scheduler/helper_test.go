package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/internal/testchild"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Helper processes re-execute this test binary through testchild. Each is
// finite: it reports readiness, waits for its stdin to close as the barrier or
// its own deadline, acts once, and exits on its own. No signal is ever sent.
const (
	helperFlag = "--scheduler-facade-helper"
	// helperHold acquires the lease, reports it, and holds it until the
	// barrier closes.
	helperHold = "hold"
	// helperProbe waits for the barrier, then tries the lease once.
	helperProbe = "probe"
	// helperMigrate waits for the barrier, then acquires the lease and
	// migrates.
	helperMigrate = "migrate"
	testDeadline  = 30 * time.Second
)

func TestMain(m *testing.M) {
	if len(os.Args) == 4 && os.Args[1] == helperFlag {
		os.Exit(runHelper(os.Args[2], os.Args[3]))
	}
	os.Exit(m.Run())
}

func runHelper(mode, root string) int {
	deadline := testDeadline
	if isCLIFixture(mode) {
		deadline = fixtureDeadline
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	store, err := taskdir.OpenStore(root)
	if err == nil {
		err = errors.Join(helperAct(ctx, mode, store), store.Close())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	return 0
}

func helperAct(ctx context.Context, mode string, store *taskdir.Store) error {
	if isCLIFixture(mode) {
		return runCLIFixture(ctx, mode, store)
	}
	if mode == probeStore {
		return probeStoreClass(ctx, store)
	}
	if mode == helperHold {
		lease, err := AcquireOwnerLease(ctx, store)
		if err != nil {
			return err
		}
		return errors.Join(testchild.AwaitRelease(ctx, "held"), lease.Close())
	}
	if err := testchild.AwaitRelease(ctx, "ready"); err != nil {
		return err
	}
	lease, err := AcquireOwnerLease(ctx, store)
	if errors.Is(err, task.ErrLockBusy) {
		return report("busy")
	}
	if err != nil {
		return err
	}
	if mode == helperMigrate {
		err = lease.Migrate(ctx)
	}
	return errors.Join(err, lease.Close(), report("acquired"))
}

func report(line string) error {
	_, err := fmt.Fprintln(os.Stdout, line)
	return err
}

// startHelper launches this test binary as one helper child for root; the
// shared testchild owner joins it at cleanup.
func startHelper(t *testing.T, mode, root string) *testchild.Child {
	t.Helper()
	return testchild.Launch(t, helperFlag, mode, root)
}
