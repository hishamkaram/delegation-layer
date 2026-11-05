package infrastructure

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestOwnerServerDispatchesSavedAndMigrationWithSelectedContext(t *testing.T) {
	for _, action := range []ownerAction{ownerRecoverSaved, ownerMigrateReferenced} {
		t.Run(string(action), func(t *testing.T) { exerciseOwnerSavedPreparation(t, action) })
	}
}

func exerciseOwnerSavedPreparation(t *testing.T, action ownerAction) {
	t.Helper()
	var calls atomic.Int32
	var expected ownerRequest
	var expectedDeadline time.Time
	check := func(ctx context.Context, op Operation, selected LaunchContext, guards ResidentGuards) {
		calls.Add(1)
		if deadline, finite := ctx.Deadline(); !finite || !deadline.Equal(expectedDeadline) {
			t.Error("handler replaced the exact authenticated deadline")
		}
		if !slices.Equal(selected.ControlEnvironment, []string{"PREPARATION=selected"}) || guards == nil || op.Intent().Inputs.Root != selected.Root {
			t.Error("host did not receive selected own context and resident grants")
		}
	}
	fault := errors.New("cleanup after verified outcome")
	host := &ownerServerTestHost{
		recoverSaved: func(ctx context.Context, op Operation, selected LaunchContext, source SavedBindingSource, guards ResidentGuards, _ DaemonWaitOwner) (Disposition, error) {
			check(ctx, op, selected, guards)
			if source != expected.Source {
				t.Error("saved locator changed")
			}
			return Disposition{Kind: DispositionStarted, Context: expected.Context}, fault
		},
		migrate: func(ctx context.Context, op Operation, selected LaunchContext, ref MigrationReference, guards ResidentGuards, _ DaemonWaitOwner) (Disposition, MigrationResult, error) {
			check(ctx, op, selected, guards)
			if ref != expected.Migration {
				t.Error("migration locator changed")
			}
			return Disposition{Kind: DispositionStarted, Context: expected.Context}, MigrationCompleted, fault
		},
	}
	f := newOwnerServerFixture(t, host)
	selected := contextForJournal(t, f.entry.journal)
	selected.OS = f.entry.operation.context.OS
	selected.ControlEnvironment = []string{"PREPARATION=selected"}
	ref, _, err := f.entry.journal.stageContext(t.Context(), selected)
	if err != nil {
		t.Fatal(err)
	}
	expected = f.request(t, action)
	expected.Context = ref
	expectedDeadline, err = canonicalStartupTime(expected.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	if action == ownerRecoverSaved {
		expected.Source = SavedBindingSource{RootID: expected.RootID, Kind: SavedBindingTaskMetadata, OwnerID: strings.Repeat("a", 32), BindingSHA256: ownerWireDigest}
	} else {
		expected.Migration = MigrationReference{RootID: expected.RootID, RecordSHA256: ownerWireDigest}
	}
	f.start()
	connection := f.dial(t)
	if err = writeOwnerFrame(connection, expected); err != nil {
		t.Fatal(err)
	}
	reply := readServerReply(t, connection)
	assertOwnerSavedPreparationReply(t, action, reply, ref, calls.Load())
}

func assertOwnerSavedPreparationReply(t *testing.T, action ownerAction, reply ownerReply, ref LaunchContextReference, calls int32) {
	t.Helper()
	if calls != 1 || reply.Code != ownerFault || reply.Disposition != (Disposition{Kind: DispositionStarted, Context: ref}) {
		t.Fatalf("canonical host outcome lost: calls=%d reply=%+v", calls, reply)
	}
	if action == ownerMigrateReferenced && reply.MigrationResult != MigrationCompleted {
		t.Fatalf("cleanup fault erased migration result: %+v", reply)
	}
}

func TestOwnerServerOtherRootReadsBContextAndRequiresBPreparation(t *testing.T) {
	var calls atomic.Int32
	b := newOwnerServerFixture(t, &ownerServerTestHost{ensure: func(_ context.Context, op Operation, selected LaunchContext, _ DaemonStartGuard, _ DaemonWaitOwner) (Disposition, error) {
		calls.Add(1)
		if selected.Root != op.Intent().Inputs.Root || selected.RootID != op.Intent().Inputs.RootID {
			t.Error("B preparation received a foreign context")
		}
		return Disposition{Kind: DispositionWarmReuse}, nil
	}})
	b.start()
	a := newOwnerServerFixture(t, &ownerServerTestHost{})
	a.start()
	request := a.request(t, ownerEnsureOther)
	request.Target = originRecord{Root: b.root, RootID: b.entry.operation.Intent().Inputs.RootID}
	request.Context = b.entry.operation.defaultContextReference()
	artifacts := b.entry.operation.Artifacts()
	request.Requirements = ArtifactRequirements{RequiredRuntimeSHA256: artifacts.RunnerSHA256, RequiredClientSHA256: artifacts.ClientSHA256, RequiredDaemonSHA256: artifacts.DaemonSHA256}
	connection := a.dial(t)
	if err := writeOwnerFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	reply := readServerReply(t, connection)
	if reply.Code != ownerOK || reply.Startup != b.request(t, ownerStatus).Owner || reply.Disposition != (Disposition{}) || calls.Load() != 0 {
		t.Fatalf("B establishment ran preparation: calls=%d reply=%+v", calls.Load(), reply)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	outcome, err := RequestBoundPreparation(ctx, contextPassiveReader(t, b.entry.journal), reply.Startup, PreparationRequest{Requirements: request.Requirements, Context: request.Context, InitialResultFor: reply.InitialResultFor})
	if err != nil || outcome.Disposition.Kind != DispositionWarmReuse || calls.Load() != 1 {
		t.Fatalf("direct B preparation failed: calls=%d outcome=%v err=%v", calls.Load(), outcome, err)
	}
}

func TestOwnerServerMigrationResultSurvivesCleanClassifiedFailure(t *testing.T) {
	for _, cause := range []error{task.ErrLockBusy, ErrOwnerBusy, ErrIncompatible, ErrOwnerIncompatible} {
		t.Run(cause.Error(), func(t *testing.T) {
			var calls atomic.Int32
			f := newOwnerServerFixture(t, &ownerServerTestHost{migrate: func(context.Context, Operation, LaunchContext, MigrationReference, ResidentGuards, DaemonWaitOwner) (Disposition, MigrationResult, error) {
				calls.Add(1)
				return Disposition{Kind: DispositionWarmReuse}, MigrationCompleted, cause
			}})
			f.start()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			migration := MigrationReference{RootID: f.entry.operation.Intent().Inputs.RootID, RecordSHA256: ownerWireDigest}
			outcome, err := RequestBoundPreparation(ctx, contextPassiveReader(t, f.entry.journal), f.request(t, ownerStatus).Owner, PreparationRequest{Operation: InitialOperation{Kind: InitialMigrateReferenced, Migration: migration}, Context: f.entry.operation.defaultContextReference(), Requirements: warmOwnerRequirements(f.entry.operation.Artifacts())})
			if outcome.Disposition.Kind != DispositionWarmReuse || outcome.MigrationResult != MigrationCompleted || outcome.Condition != PreparationFault || err != nil || calls.Load() != 1 {
				t.Fatalf("classified failure lost acknowledged migration: outcome=%v calls=%d err=%v", outcome, calls.Load(), err)
			}
			var contactErr ownerContactError
			if errors.As(err, &contactErr) {
				t.Fatalf("verified migration reply became an unknown exchange: %v", err)
			}
		})
	}
}
