package control

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPublishedPreparationReplayDoesNotAdvertisePremutationRefusal(t *testing.T) {
	fixture, candidate := interruptPrivateRestartPublication(t, "block-daemon-publication")
	pending, found, err := readPrivateIdentityPublication(fixture.stateRoot)
	if err != nil || !found || pending.Binding != candidate {
		t.Fatalf("actual interrupted publication missing: %+v found=%t err=%v", pending, found, err)
	}
	published, found, err := ReadPrivateSupervisorBinding(fixture.stateRoot)
	if err != nil || !found || published != candidate {
		t.Fatalf("interrupted binding publication missing: %+v found=%t err=%v", published, found, err)
	}
	before := readPrivateSupervisorFile(t, privateBootstrapRecordPath(fixture.stateRoot))
	if err = os.Remove(privateDaemonIdentityPath(filepath.Dir(candidate.ConfigPath))); err != nil {
		t.Fatal(err)
	}
	err = WithBootstrapLockContext(t.Context(), fixture.stateRoot, func(ctx context.Context) error {
		client, prepared, prepareErr := PreparePublishedPrivateWithLock(ctx, fixture.stateRoot, Options{ObservationTimeout: privateTestObservationTimeout})
		if client != nil {
			t.Cleanup(func() {
				if closeErr := client.CloseAfterPending(prepareErr); closeErr != nil {
					t.Error(closeErr)
				}
			})
		}
		if !prepared {
			t.Error("replayed published generation was not reported")
		}
		return prepareErr
	})
	assertPrivatePublicationReplay(t, fixture.stateRoot, candidate, before)
	assertPrivateTransitionPostconditionError(t, err)
	if err = reconcilePrivateTestPair(t, fixture, candidate); err != nil {
		t.Fatal(err)
	}
	assertPrivateDaemonStartCount(t, fixture.stateRoot, 2)
	complete, err := PrivateSupervisorMigrationComplete(fixture.stateRoot, candidate)
	if err != nil || !complete {
		t.Fatalf("recovery did not complete the same generation: complete=%t err=%v", complete, err)
	}
}

func assertPrivatePublicationReplay(t *testing.T, stateRoot string, candidate task.SupervisorRef, bootstrapBefore []byte) {
	t.Helper()
	identity, found, err := loadPrivateDaemonIdentity(filepath.Dir(candidate.ConfigPath))
	if err != nil || !found || identity != privateDaemonIdentityForBinding(candidate) {
		t.Fatalf("replay did not publish exact identity: %+v found=%t err=%v", identity, found, err)
	}
	if _, found, err = readPrivateIdentityPublication(stateRoot); err != nil || found {
		t.Fatalf("replayed publication journal remains: found=%t err=%v", found, err)
	}
	if after := readPrivateSupervisorFile(t, privateBootstrapRecordPath(stateRoot)); !bytes.Equal(bootstrapBefore, after) {
		t.Fatal("preparation changed the exact unfinished restart intent")
	}
	complete, err := PrivateSupervisorMigrationComplete(stateRoot, candidate)
	if err != nil || complete {
		t.Fatalf("unfinished restart was accepted as complete: complete=%t err=%v", complete, err)
	}
	assertPrivateDaemonStartCount(t, stateRoot, 2)
}
