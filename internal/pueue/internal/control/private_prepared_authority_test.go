package control

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// PM-C1: the observed generation is still draining. Exclusive repair and
// scheduling, completion, cleanup, and history traversal retain strict gates.
func TestPreparedPrivateMigrationKeepsExclusiveConsumersStrict(t *testing.T) {
	fixture := newBusyPreparedPrivateObservation(t)
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	err := WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		if err := fixture.legacy.EnsurePrivateSupervisorBindingWithLock(ctx, fixture.stateRoot); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
			t.Errorf("Ensure accepted draining generation: %v", err)
		}
		client, found, err := PreparePublishedPrivateWithLock(ctx, fixture.stateRoot, fixture.legacy.options)
		if client != nil {
			if closeErr := client.CloseAfterPending(err); closeErr != nil {
				t.Error(closeErr)
			}
		}
		if !found || !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
			t.Errorf("PreparePublished accepted draining generation: found=%t err=%v", found, err)
		}
		binding := fixture.legacy.Binding()
		client, _, ready, err := ProbePrivateGroupsWithBootstrapLock(ctx, binding.ClientExecutable, binding.DaemonExecutable, fixture.stateRoot, fixture.legacy.options)
		if client != nil {
			if closeErr := client.CloseAfterPending(err); closeErr != nil {
				t.Error(closeErr)
			}
		}
		if err != nil || ready {
			t.Errorf("group repair accepted draining generation: ready=%t err=%v", ready, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertPreparedPrivateObservationUnchanged(t, fixture, before)
}

func TestPreparedPrivateMigrationKeepsHistoryTraversalStrict(t *testing.T) {
	fixture := newBusyPreparedPrivateObservation(t)
	older := fixture.legacy.Binding()
	older.ConfigDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	_, _, err := ResolvePrivateSupervisorSuccessor(t.Context(), fixture.stateRoot, older)
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Errorf("history traversal accepted incomplete current generation: %v", err)
	}
	assertPreparedPrivateObservationUnchanged(t, fixture, before)
}

func TestPreparedPrivateMigrationResumesDifferentSuccessorAfterDrain(t *testing.T) {
	fixture := newBusyPreparedPrivateObservation(t)
	writePreparedPrivateQueue(t, fixture, false)
	if err := migratePreparedPrivateWriter(t, fixture, fixture.successor); err != nil {
		t.Fatalf("prepared different-generation migration could not resume: %v", err)
	}
	current, found, err := ReadPrivateSupervisorBinding(fixture.stateRoot)
	if err != nil || !found || current != fixture.successor.Binding() {
		t.Errorf("resumed migration published another successor: found=%t err=%v", found, err)
	}
	assertPreparedPrivateIntentAbsent(t, fixture)
	if complete, err := PrivateSupervisorMigrationComplete(fixture.stateRoot, current); err != nil || !complete {
		t.Errorf("resumed migration did not finish: complete=%t err=%v", complete, err)
	}
	if _, found, err := loadPrivateMigrationHistory(filepath.Join(fixture.stateRoot, privateSupervisorDirectory)); err != nil || !found {
		t.Errorf("resumed different-generation migration lost history: found=%t err=%v", found, err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
}

func TestPrivateCompletionRetainsBootstrapErrorPrecedence(t *testing.T) {
	fixture := newPreparedPrivateObservationPair(t)
	withPreparedPrivateFaultLock(t, fixture, func() {
		if err := markPrivateBootstrapPending(fixture.stateRoot); err != nil {
			t.Fatal(err)
		}
		writePrivateSuccessorTestBytes(t, privateIdentityPublicationPath(fixture.stateRoot), []byte("{"))
	})
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	for _, allowAlias := range []bool{false, true} {
		complete, err := privateSupervisorMigrationComplete(fixture.stateRoot, fixture.legacy.Binding(), allowAlias)
		if err != nil || complete {
			t.Errorf("pending bootstrap no longer takes precedence: allowAlias=%t complete=%t err=%v", allowAlias, complete, err)
		}
	}
	assertPreparedPrivateObservationUnchanged(t, fixture, before)
}
