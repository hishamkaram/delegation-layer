package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRootAdmissionPinObservationRequiresSharedAuthority(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	for _, lease := range []*RootAdmissionLease{nil, {}} {
		if _, err := lease.PinRootContext(t.Context()); err == nil {
			t.Fatal("unacquired resource exposed an admission pin")
		}
	}
	exclusive := fixture.exclusive(t, fixture.store)
	if _, err := exclusive.PinRootContext(t.Context()); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatalf("EX exposed an SH pin: %v", err)
	}
	must(t, exclusive.Release())
	lease := fixture.shared(t, fixture.store)
	expected, err := fixture.store.PinRootContext(t.Context())
	must(t, err)
	before, err := lease.PinRootContext(t.Context())
	must(t, err)
	copy := duplicateAdmission(t, fixture, lease)
	after, err := lease.PinRootContext(t.Context())
	must(t, err)
	if !before.Equal(expected) || !after.Equal(expected) {
		t.Fatal("transfer changed the resource's native root identity")
	}
	must(t, lease.Release())
	if _, err = lease.PinRootContext(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("released resource exposed an admission pin: %v", err)
	}
	requireRootAdmissionBusy(t, fixture, fixture.store)
	must(t, copy.Close())
}

func TestRootAdmissionPinObservationRechecksContextAndNamedInode(t *testing.T) {
	fixture := newRootAdmissionFixture(t)
	lease := fixture.shared(t, fixture.store)
	copy := duplicateAdmission(t, fixture, lease)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := lease.PinRootContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("pin observation lost caller cancellation: %v", err)
	}
	path := filepath.Join(fixture.store.Root, RootAdmissionLockName)
	saved := path + ".pin-fixture"
	must(t, os.Rename(path, saved))
	must(t, os.WriteFile(path, nil, 0o600))
	_, err := lease.PinRootContext(t.Context())
	must(t, os.Remove(path))
	must(t, os.Rename(saved, path))
	if !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("pin observation accepted a replacement lock inode: %v", err)
	}
	_, err = lease.PinRootContext(t.Context())
	must(t, err)
	must(t, lease.Release())
	must(t, copy.Close())
}
