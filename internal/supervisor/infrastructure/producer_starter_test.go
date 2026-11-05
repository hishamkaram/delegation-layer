package infrastructure

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
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
	self, err := lifetime.ObserveProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if starter.provenance != (StartupProvenance{Kind: StarterOutside, PID: self.PID(), StartToken: self.StartToken()}) {
		t.Fatal("outside snapshot selected another process or owner provenance")
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
	prepare := func(ctx context.Context, store *taskdir.Store) (Artifacts, error) {
		if !source.entry.mu.TryLock() {
			t.Fatal("source resident guard retained across target work")
		}
		source.entry.mu.Unlock()
		if leaseErr := source.entry.lease.WithHeld(ctx, func() error { return nil }); leaseErr != nil {
			return Artifacts{}, leaseErr
		}
		return target.host.PrepareOutsideArtifacts(ctx, store)
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
	if starter.provenance.Kind != StarterOwner || starter.provenance.Root != intent.Inputs.Root || starter.provenance.RootID != intent.Inputs.RootID || starter.provenance.Epoch != intent.Epoch {
		t.Fatal("snapshot did not derive the actual resident provenance")
	}
	if attempt.intent.Starter != starter.provenance {
		t.Fatal("target startup rewrote authenticated owner provenance")
	}
	if attempt.intent.Inputs.RootID == starter.provenance.RootID || attempt.NativeCompletion() != nil {
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

func TestProducerSameRootStarterRefusedBeforeTargetPreparation(t *testing.T) {
	requireComponentEntryTestHost(t)
	source := newOwnerServerFixture(t, &ownerServerTestHost{})
	starter, err := source.entry.captureOwnerProducer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	before, err := source.entry.journal.control.RecordNamesContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	artifacts := source.entry.operation.Artifacts()
	request := EstablishmentRequest{
		Context: source.entry.operation.context.Clone(), Deadline: time.Now().Add(time.Minute),
		Requirements: ArtifactRequirements{
			RequiredRuntimeSHA256: artifacts.RunnerSHA256,
			RequiredClientSHA256:  artifacts.ClientSHA256, RequiredDaemonSHA256: artifacts.DaemonSHA256,
		},
	}
	preparations := 0
	prepare := func(context.Context, *taskdir.Store) (Artifacts, error) {
		preparations++
		return artifacts, nil
	}
	attempt, err := establishCaptured(t.Context(), source.entry.store, request, capturedEstablishment{starter: starter, prepare: prepare}, producerNative{})
	ownProducerAttempt(t, attempt)
	if !errors.Is(err, ErrIdentity) || attempt != nil || preparations != 0 {
		t.Fatalf("same-root owner reached target election/preparation: preparations=%d err=%v", preparations, err)
	}
	after, err := source.entry.journal.control.RecordNamesContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(before)
	slices.Sort(after)
	if !slices.Equal(before, after) {
		t.Fatal("same-root owner changed target journal")
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
	if err == nil || starter.provenance != (StartupProvenance{}) || starter.environment != nil {
		t.Fatalf("missing canonical endpoint minted a starter: %v", err)
	}
}
