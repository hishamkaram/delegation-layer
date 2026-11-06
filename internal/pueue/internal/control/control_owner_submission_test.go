package control

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestControlOwnerManagedStagedSubmission(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	ctx := t.Context()
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
	if client.options.ControlOwnerExecutable != os.Args[0] || clone.options.ControlOwnerExecutable != os.Args[0] || client.ControlTimeout() != PrivateControlTimeout(Options{ObservationTimeout: time.Second}) || clone.ControlTimeout() != PrivateControlTimeout(Options{ObservationTimeout: 2 * time.Second}) {
		t.Fatal("derived views lost selected owner/budget or changed original")
	}
	var absent *Client
	if absent.ControlTimeout() != 0 {
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
}
