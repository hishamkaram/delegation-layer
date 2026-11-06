package infrastructure

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestProducerOutsideStarterOwnsCapturedEnvironment(t *testing.T) {
	requireComponentEntryTestHost(t)
	environment := os.Environ()
	environment = append(environment, "PRODUCER_CAPTURE_FIXTURE=original")
	starter, err := captureOutsideProducer(t.Context(), environment)
	if err != nil {
		t.Fatal(err)
	}
	environment[len(environment)-1] = "PRODUCER_CAPTURE_FIXTURE=changed"
	if !slices.Contains(starter.environment, "PRODUCER_CAPTURE_FIXTURE=original") || slices.Contains(starter.environment, "PRODUCER_CAPTURE_FIXTURE=changed") {
		t.Fatal("starter borrowed mutable caller environment")
	}
}

func TestProducerResidentSnapshotUsesSharedAcceptanceWithoutSourceGuards(t *testing.T) {
	requireComponentEntryTestHost(t)
	source := newOwnerServerFixture(t, &ownerServerTestHost{})
	starter, err := source.entry.captureOwnerProducer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	target := producerFixture(t)
	releaseProducerFixtureLease(t, target)
	refused := errors.New("fixture refuses native Start after durable acceptance")
	starts := 0
	prepare := func(ctx context.Context, store *taskdir.Store) (string, error) {
		if !source.entry.mu.TryLock() {
			t.Fatal("source resident guard retained across target work")
		}
		source.entry.mu.Unlock()
		if leaseErr := source.entry.lease.WithHeld(ctx, func() error { return nil }); leaseErr != nil {
			return "", leaseErr
		}
		return target.host.PrepareOutsideRunner(ctx, store)
	}
	attempt, err := establishCaptured(t.Context(), target.journal.store, target.request,
		capturedEstablishment{starter: starter, prepare: prepare}, producerNative{start: func(*exec.Cmd) error {
			starts++
			return refused
		}})
	ownProducerAttempt(t, attempt)
	if !errors.Is(err, refused) || starts != 1 || attempt == nil || attempt.Reference() == (StartupReference{}) {
		t.Fatalf("shared acceptance/start ownership: starts=%d err=%v", starts, err)
	}
	intent := source.entry.operation.Intent()
	if attempt.intent.Inputs.RootID == intent.Inputs.RootID || attempt.NativeCompletion() != nil {
		t.Fatal("target used source identity or invented a native child after refused Start")
	}
}

func TestProducerResidentSnapshotRequiresActualOpenGrant(t *testing.T) {
	requireComponentEntryTestHost(t)
	source := newOwnerServerFixture(t, &ownerServerTestHost{})
	source.entry.mu.Lock()
	grant := source.entry.grant
	source.entry.grant = nil
	source.entry.mu.Unlock()
	if _, err := source.entry.captureOwnerProducer(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("ungranted entry minted a starter: %v", err)
	}
	source.entry.mu.Lock()
	source.entry.grant = grant
	source.entry.mu.Unlock()
	if err := source.server.stopAcceptAndDrain(t.Context()); err != nil {
		t.Fatal(err)
	}
	source.server = nil
	if err := source.entry.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := source.entry.captureOwnerProducer(t.Context()); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed resident minted a starter: %v", err)
	}
}

func TestProducerSameRootOwnerRequestRefusedBeforeTargetPreparation(t *testing.T) {
	requireComponentEntryTestHost(t)
	source := newOwnerServerFixture(t, &ownerServerTestHost{})
	before, err := source.entry.journal.control.RecordNamesContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	request := source.request(t, ownerEnsureOther)
	// A different claimed ID passes the wire's same-owner rejection. The actual
	// source path must still fail target pin validation after authentication.
	request.Target = originRecord{Root: source.root, RootID: strings.Repeat("f", 32)}
	request.Context = source.entry.operation.defaultContextReference()
	source.start()
	connection := source.dial(t)
	if err = writeOwnerFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	reply := readServerReply(t, connection)
	if reply.Code != ownerFault || reply.Startup != (StartupReference{}) {
		t.Fatalf("same-root owner request reached target establishment: %+v", reply)
	}
	after, err := source.entry.journal.control.RecordNamesContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(before)
	slices.Sort(after)
	if !slices.Equal(before, after) {
		t.Fatal("same-root owner request changed target journal")
	}
}

func TestProducerResidentSnapshotRevalidatesPublishedEndpoint(t *testing.T) {
	requireComponentEntryTestHost(t)
	source := newOwnerServerFixture(t, &ownerServerTestHost{})
	path := source.entry.endpoint.Identity().Path
	moved := path + ".fixture-displaced"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Rename(moved, path); err != nil {
			t.Errorf("restore owned endpoint before resident cleanup: %v", err)
		}
	})
	starter, err := source.entry.captureOwnerProducer(t.Context())
	if err == nil || starter.environment != nil {
		t.Fatalf("missing canonical endpoint minted a starter: %v", err)
	}
}
