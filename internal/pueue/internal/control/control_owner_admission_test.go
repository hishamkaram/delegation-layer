package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func admissionTestLease(t *testing.T, store *taskdir.Store) *taskdir.RootAdmissionLease {
	t.Helper()
	lease, err := store.AcquireRootAdmissionContext(t.Context())
	t.Cleanup(func() {
		if releaseErr := lease.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func TestControlAdmissionManagedOrdinaryUsesActualRootResource(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	lease := admissionTestLease(t, fixture.store)
	ctx := WithControlLease(t.Context(), lease)
	permit, err := fixture.taskDir.PrepareSubmission(fake.client.Binding())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { releaseStagedSubmissionPermit(t, permit) })
	fake.client.options.ControlOwnerExecutable = os.Args[0]
	if fake.client.privateConstruction != nil {
		t.Fatal("fixture must exercise an external bound supervisor")
	}
	launch := Launch{RunnerExecutable: fake.executable, RootPath: fixture.store.Root, Supervised: true}
	observation, err := fake.client.SubmitStaged(ctx, permit, launch, func(candidate Observation) error {
		receipt, receiptErr := candidate.Receipt()
		if receiptErr != nil {
			return receiptErr
		}
		return fixture.taskDir.RecordSupervisorReceipt(*receipt)
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := fixture.taskDir.ReadSupervisorReceipt()
	if err != nil || receipt == nil || receipt.NumericTaskID != observation.NumericTaskID {
		t.Fatalf("actual staged receipt missing: %+v err=%v", receipt, err)
	}
	if len(stagedInvocations(t, fake, "add")) != 1 || len(stagedInvocations(t, fake, "enqueue")) != 1 {
		t.Fatalf("staged protocol cardinality changed: %+v", readInvocations(t, fake.logPath))
	}
}

func TestControlAdmissionMissingResourceDoesNotConsumePermit(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	_, permit, _, launch := prepareStagedOrdinarySubmission(t, fake)
	fake.client.options.ControlOwnerExecutable = os.Args[0]
	before := len(readInvocations(t, fake.logPath))
	_, err := fake.client.SubmitStaged(t.Context(), permit, launch, func(Observation) error { return nil })
	if !errors.Is(err, ErrRootAdmissionRequired) || permit.IsConsumed() {
		t.Fatalf("missing Root SH admitted work: consumed=%t err=%v", permit.IsConsumed(), err)
	}
	if after := len(readInvocations(t, fake.logPath)); after != before {
		t.Fatalf("missing resource started a command: before=%d after=%d", before, after)
	}
}

func TestControlAdmissionRejectsWrongRootMultipleAndReleasedResources(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	lease := admissionTestLease(t, fixture.store)
	fake.client.options.ControlOwnerExecutable = os.Args[0]
	ctx := WithControlLease(t.Context(), lease)
	otherID, err := randomid.NewRootID()
	if err != nil {
		t.Fatal(err)
	}
	if err = fake.client.checkAdmissionResource(ctx, otherID, fixture.store.Root); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("wrong RootID accepted: %v", err)
	}
	idempotent := WithControlLease(ctx, lease)
	if len(controlLeaseContextFrom(idempotent).extra) != 1 {
		t.Fatal("same actual root resource was attached twice")
	}
	second := admissionTestLease(t, fixture.store)
	multiple := WithControlLease(ctx, second)
	if err = fake.client.checkAdmissionResource(multiple, fixture.store.RootID, fixture.store.Root); !errors.Is(err, ErrRootAdmissionRequired) {
		t.Fatalf("multiple actual resources accepted: %v", err)
	}
	if err = lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err = fake.client.checkAdmissionResource(ctx, fixture.store.RootID, fixture.store.Root); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("released resource accepted/lost fault: %v", err)
	}
}

func TestControlAdmissionRejectsEXAndNativeRootSubstitution(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	fake.client.options.ControlOwnerExecutable = os.Args[0]
	exclusive, err := fixture.store.TryAcquireRootAdmissionExclusiveContext(t.Context())
	t.Cleanup(func() {
		if releaseErr := exclusive.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = fake.client.checkAdmissionResource(WithControlLease(t.Context(), exclusive), fixture.store.RootID, fixture.store.Root); !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatalf("EX became forwardable SH: %v", err)
	}
	if err = exclusive.Release(); err != nil {
		t.Fatal(err)
	}
	shared := admissionTestLease(t, fixture.store)
	other := newTaskFixture(t, fake.client.Binding())
	rootData, err := os.ReadFile(filepath.Join(fixture.store.Root, "root.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(other.store.Root, "root.json"), rootData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = fake.client.checkAdmissionResource(WithControlLease(t.Context(), shared), fixture.store.RootID, other.store.Root); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("same-ID different native root accepted: %v", err)
	}
}

func TestControlAdmissionResourceRefusesInProcessFallback(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	ctx := WithControlLease(t.Context(), admissionTestLease(t, fixture.store))
	before := len(readInvocations(t, fake.logPath))
	consumed := false
	result, err := fake.client.command(ctx, func() error { consumed = true; return nil }, "status", "--json")
	if result.Started || consumed || !errors.Is(err, ErrRootAdmissionRequired) {
		t.Fatalf("in-process fallback survived Root SH: started=%t consumed=%t err=%v", result.Started, consumed, err)
	}
	if len(readInvocations(t, fake.logPath)) != before {
		t.Fatal("fallback entered the CLI")
	}
}

func TestControlAdmissionSelectsResourceDuringDuplication(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	lease := admissionTestLease(t, fixture.store)
	fake.client.options.ControlOwnerExecutable = os.Args[0]
	decoy, err := os.OpenFile(filepath.Join(t.TempDir(), "decoy"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := decoy.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	ctx := WithControlLease(t.Context(), qualifiedFixtureLease{files: []*os.File{decoy}})
	ctx = WithControlLease(ctx, lease)
	scoped, err := fake.client.admissionCommandContext(ctx, controlAdmissionEnqueue, fixture.store.RootID, "")
	if err != nil {
		t.Fatal(err)
	}
	duplicates, err := duplicateControlLeaseFiles(scoped)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := closeControlLeaseFiles(duplicates.files); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if duplicates.admission == nil || duplicates.admission.LeaseIndex != 1 {
		t.Fatalf("resource selected by position rather than actual duplication: %+v", duplicates.admission)
	}
	request := controlOwnerRequest{Admission: duplicates.admission, Arguments: []string{fake.executable, "-c", fake.configPath, "enqueue", "7"}}
	if err = validateControlOwnerForwarding(request); err != nil {
		t.Fatal(err)
	}
	selected, err := controlOwnerForwardedFiles(request, duplicates.files)
	if err != nil || len(selected) != 1 || selected[0] != duplicates.files[1] {
		t.Fatalf("forwarded unrelated/merged leases: len=%d err=%v", len(selected), err)
	}
	wrong := *duplicates.admission
	wrong.LeaseIndex = 0
	request.Admission = &wrong
	if _, selectErr := controlOwnerForwardedFiles(request, duplicates.files); selectErr == nil {
		t.Fatal("decoy accepted as root admission inode")
	}
	request.Admission = duplicates.admission
	request.Shutdown = &controlOwnerShutdown{}
	if err = validateControlOwnerForwarding(request); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("co-forwarding accepted: %v", err)
	}
}

func TestControlAdmissionManagedDirectCompatibilityIsUnsupported(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	_, permit, _, launch := prepareStagedOrdinarySubmission(t, fake)
	fake.client.options.ControlOwnerExecutable = os.Args[0]
	before := len(readInvocations(t, fake.logPath))
	_, err := fake.client.SubmitWithGate(context.Background(), permit, launch, nil)
	if !errors.Is(err, ErrManagedAdmissionUnsupported) || permit.IsConsumed() {
		t.Fatalf("direct compatibility add survived managed mode: consumed=%t err=%v", permit.IsConsumed(), err)
	}
	if len(readInvocations(t, fake.logPath)) != before {
		t.Fatal("unsupported direct add started CLI work")
	}
}

func TestControlAdmissionClientViewsFollowImmutableClone(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	client, err := NewClient(fake.client.Binding(), Options{ObservationTimeout: time.Second, ControlOwnerExecutable: os.Args[0]})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	clone, err := client.WithObservationTimeout(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !client.UsesControlOwner() || !clone.UsesControlOwner() || client.ControlTimeout() != PrivateControlTimeout(Options{ObservationTimeout: time.Second}) || clone.ControlTimeout() != PrivateControlTimeout(Options{ObservationTimeout: 2 * time.Second}) {
		t.Fatal("derived views lost selected owner/budget or changed original")
	}
	var absent *Client
	if absent.ControlTimeout() != 0 || absent.UsesControlOwner() {
		t.Fatal("nil client claims a usable owner or budget")
	}
}

type admissionUncomparableLease []int

func (admissionUncomparableLease) DuplicateControlLeaseFiles() ([]*os.File, error) { return nil, nil }

func TestControlAdmissionGenericAttachmentRemainsUnchanged(t *testing.T) {
	generic := admissionUncomparableLease{1}
	ctx := WithControlLease(WithControlLease(t.Context(), generic), generic)
	if len(controlLeaseContextFrom(ctx).extra) != 2 {
		t.Fatal("generic attachment semantics changed")
	}
	if _, count := rootAdmissionCandidate(ctx); count != 0 {
		t.Fatal("generic lease minted root admission")
	}
}
