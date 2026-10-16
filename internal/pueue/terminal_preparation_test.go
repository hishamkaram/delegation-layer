package pueue

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

func TestPrivateCleanupProtectsPreparationAndReleasesIdentityBeforeAuthorization(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	writeLargePrivateQueue(t, fixture.stateRoot, true)
	var prepared atomic.Bool
	var snapshots atomic.Int32
	client.options.Observer = func(event CommandEvent) {
		if event.Stage == "entry" && slices.Contains(event.Argv, "status") {
			snapshots.Add(1)
		}
	}
	client.options.PrivateTerminalEvidencePreparer = func(ctx context.Context, root string, _ task.SupervisorRef, jobs []Job) error {
		if err := checkPreparationIdentityLock(ctx, root, true); err != nil {
			return err
		}
		if len(jobs) != 200 {
			return ErrCleanupAuthorization
		}
		prepared.Store(true)
		return nil
	}
	client.options.SuccessfulCleanupAuthorizer = func(ctx context.Context, root string, _ task.SupervisorRef, jobs []Job) (PrivateQueueCleanupPlan, error) {
		if !prepared.Load() || len(jobs) != 200 {
			return PrivateQueueCleanupPlan{}, ErrCleanupAuthorization
		}
		if err := checkPreparationIdentityLock(ctx, root, false); err != nil {
			return PrivateQueueCleanupPlan{}, err
		}
		return PrivateQueueCleanupPlan{RemoveIDs: []int64{0}}, nil
	}
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		_, passErr := client.cleanSuccessfulTasksPass(ctx, false)
		return passErr
	})
	if err != nil || !prepared.Load() || snapshots.Load() != 2 {
		t.Fatalf("cleanup did not preserve preparation/authorization ordering and two snapshots: prepared=%t snapshots=%d err=%v", prepared.Load(), snapshots.Load(), err)
	}
}

func checkPreparationIdentityLock(ctx context.Context, root string, held bool) error {
	lock, err := acquireExistingSupervisorLock(ctx, filepath.Join(root, privateSupervisorDirectory, privateQueueIdentityLockName), unix.LOCK_EX|unix.LOCK_NB)
	if held && errors.Is(err, errSupervisorLockBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	closeErr := closeBootstrapLock(lock)
	if held {
		return errors.Join(ErrCleanupAuthorization, closeErr)
	}
	return closeErr
}

func TestPrivateMigrationPreparesPredecessorBeforeAuthorization(t *testing.T) {
	fixture := newPrivateSupervisorTestFixture(t)
	client := bindPrivateForTest(t, fixture.stateRoot, fixture.clientPath, fixture.daemonPath)
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	var prepared bool
	client.options.PrivateTerminalEvidencePreparer = func(_ context.Context, root string, binding task.SupervisorRef, _ []Job) error {
		if root != fixture.stateRoot || binding != client.Binding() {
			return ErrMigrationAuthorization
		}
		prepared = true
		return nil
	}
	client.options.PrivateMigrationAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) error {
		if !prepared {
			return ErrMigrationAuthorization
		}
		return nil
	}
	if err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		return client.authorizePrivateMigrationQueue(ctx, fixture.stateRoot, client)
	}); err != nil || !prepared {
		t.Fatalf("migration authorization preceded evidence preparation: prepared=%t err=%v", prepared, err)
	}
}
