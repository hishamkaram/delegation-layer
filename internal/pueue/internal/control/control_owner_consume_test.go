package control

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

// This extra owns no admission authority. It deterministically releases the
// actual resource after the canonical Add checks but before its duplication.
type admissionReleaseDuringDup struct {
	resource   *taskdir.LifetimeOwnerLease
	original   *os.File
	armed      bool
	started    bool
	duplicates []*os.File
}

func (l *admissionReleaseDuringDup) observe(event CommandEvent) {
	if len(event.Argv) < 5 || event.Argv[3] != "add" || event.Argv[4] != "--escape" {
		return
	}
	if event.Stage == "entry" {
		l.armed = true
	}
	if event.Stage == "started" {
		l.started = true
	}
}

func (l *admissionReleaseDuringDup) DuplicateControlLeaseFiles() ([]*os.File, error) {
	if !l.armed {
		return nil, nil
	}
	if l.resource != nil {
		if err := l.resource.Release(); err != nil {
			return nil, err
		}
	}
	duplicate, err := duplicateCloseOnExec(l.original, "consume-order-decoy")
	if err != nil {
		return nil, err
	}
	l.duplicates = append(l.duplicates, duplicate)
	return []*os.File{duplicate}, nil
}

func TestControlAdmissionReleasedDuringDupDoesNotConsumePermit(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	fixture, permit, _, launch := prepareStagedOrdinarySubmission(t, fake)
	resource := consumeTestLifetimeLease(t, fixture.store)
	original := consumeTestDecoy(t)
	interposer := &admissionReleaseDuringDup{resource: resource, original: original}
	ctx := WithControlLease(WithControlLease(t.Context(), interposer), resource)
	fake.client.options.ControlOwnerExecutable = os.Args[0]
	fake.client.options.Observer = interposer.observe
	_, err := fake.client.SubmitStaged(ctx, permit, launch, func(Observation) error { return nil })
	if !errors.Is(err, task.ErrPermitAlreadyUsed) || errors.Is(err, ErrSubmissionUncertain) {
		t.Errorf("released actual lease did not refuse before Add: %v", err)
	}
	if permit.IsConsumed() {
		t.Error("failed actual lease duplication spent the submission permit")
	}
	if interposer.started || len(stagedInvocations(t, fake, "add")) != 0 {
		t.Error("lease preparation failure started Add")
	}
	if len(interposer.duplicates) != 1 {
		t.Errorf("interposer did not reach exactly the Add duplication: %d", len(interposer.duplicates))
	}
	assertConsumeDuplicatesClosed(t, interposer.duplicates)
	// Close joins the command work owner, including the already-completed failed
	// Pending. The actual resource must also be free after its successful Release.
	if closeErr := fake.client.Close(); closeErr != nil {
		t.Error(closeErr)
	}
	control, openErr := fixture.store.OpenInfrastructureContext(t.Context(), true)
	if openErr != nil {
		t.Fatal(openErr)
	}
	candidate, acquireErr := control.TryAcquireLifetimeOwnerContext(t.Context())
	if closeErr := errors.Join(candidate.Release(), control.Close()); closeErr != nil {
		t.Error(closeErr)
	}
	if acquireErr != nil {
		t.Errorf("released lifetime resource still busy: %v", acquireErr)
	}
}

func TestControlOwnerConsumeFailureClosesPreparedResources(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	resource := consumeTestLifetimeLease(t, fixture.store)
	interposer := &admissionReleaseDuringDup{original: consumeTestDecoy(t), armed: true}
	ctx := WithControlLease(WithControlLease(t.Context(), resource), interposer)
	fake.client.options.ControlOwnerExecutable = os.Args[0]
	cause := errors.New("canonical consume refused")
	result, err := fake.client.command(ctx, func() error {
		if len(interposer.duplicates) != 1 {
			t.Error("consume ran before descriptor preparation")
		}
		return cause
	}, "status")
	if !errors.Is(err, cause) || result.Started {
		t.Errorf("consume refusal lost its cause or started a command: %+v err=%v", result, err)
	}
	if len(stagedInvocations(t, fake, "status")) != 0 {
		t.Error("consume refusal entered the CLI")
	}
	assertConsumeDuplicatesClosed(t, interposer.duplicates)
	if closeErr := fake.client.Close(); closeErr != nil {
		t.Error(closeErr)
	}
	control, openErr := fixture.store.OpenInfrastructureContext(t.Context(), true)
	if openErr != nil {
		t.Fatal(openErr)
	}
	candidate, acquireErr := control.TryAcquireLifetimeOwnerContext(t.Context())
	if closeErr := errors.Join(candidate.Release(), control.Close()); closeErr != nil {
		t.Error(closeErr)
	}
	if !errors.Is(acquireErr, task.ErrLockBusy) {
		t.Errorf("closing copies released caller's lifetime resource: %v", acquireErr)
	}
}

func consumeTestDecoy(t *testing.T) *os.File {
	t.Helper()
	original, err := os.OpenFile(filepath.Join(t.TempDir(), "decoy"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := original.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return original
}

func assertConsumeDuplicatesClosed(t *testing.T, descriptors []*os.File) {
	t.Helper()
	for _, descriptor := range descriptors {
		if _, err := unix.FcntlInt(descriptor.Fd(), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
			t.Errorf("prepared descriptor remains reachable: %v", err)
		}
	}
}

func consumeTestLifetimeLease(t *testing.T, store *taskdir.Store) *taskdir.LifetimeOwnerLease {
	t.Helper()
	control, err := store.OpenInfrastructureContext(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := control.AcquireLifetimeOwner(t.Context())
	t.Cleanup(func() {
		if closeErr := errors.Join(lease.Release(), control.Close()); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}
