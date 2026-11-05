package infrastructure

import (
	"context"
	"errors"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestOwnerClientProbesAuthenticatedResidentAfterStartupDeadline(t *testing.T) {
	f := newOwnerContactFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	join := f.serve(t, func(connection *net.UnixConn, request ownerRequest) error {
		if request.Action != ownerStatus || request.Owner != f.auth.request.Owner {
			return ErrIdentity
		}
		return writeOwnerFrame(connection, ownerServerReply(request, ownerOK))
	})
	reference, err := ProbeOwner(ctx, f.reader)
	if err != nil || reference != f.auth.request.Owner {
		t.Fatalf("authenticated current owner: reference=%v err=%v", reference, err)
	}
	if err = join(); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerClientOwnPreparationPreservesSelectedContext(t *testing.T) {
	f := newOwnerContactFixture(t)
	selected := contextForJournal(t, f.journal)
	selected.ControlEnvironment = append(selected.ControlEnvironment, "OWNER_CLIENT_SELECTED=one")
	reference, err := StageLaunchContext(t.Context(), f.auth.store, selected)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	join := f.serve(t, func(connection *net.UnixConn, request ownerRequest) error {
		if request.Context != reference || request.Action != ownerEnsureOwn {
			return ErrIdentity
		}
		reply := ownerServerReply(request, ownerOK)
		reply.Target = originRecord{Root: f.journal.pin.Root(), RootID: f.journal.pin.RootID()}
		reply.Startup = request.Owner
		reply.Disposition = Disposition{Kind: DispositionWarmReuse}
		return writeOwnerFrame(connection, reply)
	})
	outcome, err := RequestBoundPreparation(ctx, f.reader, f.auth.request.Owner, PreparationRequest{Context: reference, Requirements: warmOwnerRequirements(f.auth.intent.Inputs.Artifacts)})
	disposition := outcome.Disposition
	if err != nil || outcome.Condition != PreparationOK || disposition != (Disposition{Kind: DispositionWarmReuse}) {
		t.Fatalf("own preparation: disposition=%v err=%v", disposition, err)
	}
	if err = join(); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerClientDoesNotReplayBusyOrUnknownContact(t *testing.T) {
	for _, test := range []struct {
		name    string
		unknown bool
	}{{"busy", false}, {"unknown", true}} {
		t.Run(test.name, func(t *testing.T) { exerciseOwnerClientContactOutcome(t, test.unknown) })
	}
}

func exerciseOwnerClientContactOutcome(t *testing.T, unknown bool) {
	t.Helper()
	f := newOwnerContactFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var received atomic.Int32
	join := f.serve(t, func(connection *net.UnixConn, request ownerRequest) error {
		received.Add(1)
		if unknown {
			return nil
		}
		return writeOwnerFrame(connection, ownerServerReply(request, ownerBusy))
	})
	outcome, err := RequestBoundPreparation(ctx, f.reader, f.auth.request.Owner, PreparationRequest{Requirements: warmOwnerRequirements(f.auth.intent.Inputs.Artifacts)})
	if unknown {
		if outcome != (PreparationOutcome{}) || err == nil {
			t.Fatalf("unknown contact became success: outcome=%v err=%v", outcome, err)
		}
		requireOwnerContactPhase(t, err, ownerContactMayHaveWritten, true)
	} else if err != nil || outcome.Condition != PreparationBusy || outcome.Disposition != (Disposition{}) {
		t.Fatalf("verified busy outcome lost: outcome=%v err=%v", outcome, err)
	}
	if err = join(); err != nil {
		t.Fatal(err)
	}
	if received.Load() != 1 {
		t.Fatalf("received %d requests", received.Load())
	}
	requireNoOwnerContactConnection(t, f.auth.listener)
}

func TestOwnerClientRequiresFiniteContextBeforeContact(t *testing.T) {
	f := newOwnerContactFixture(t)
	for _, ctx := range []context.Context{nil, context.Background()} {
		if _, err := ProbeOwner(ctx, f.reader); !errors.Is(err, os.ErrInvalid) {
			t.Fatalf("unbounded probe: %v", err)
		}
	}
	requireNoOwnerContactConnection(t, f.auth.listener)
}

// This is transport composition only. Source ownership and recovery eligibility
// remain the receiving resident's canonical read, never these fixture locators.
func TestOwnerClientPreservesSavedAndMigrationSubjects(t *testing.T) {
	for _, test := range []struct {
		name      string
		migration bool
	}{{"saved", false}, {"migration", true}} {
		t.Run(test.name, func(t *testing.T) { exerciseOwnerClientSavedSubject(t, test.migration) })
	}
}

func exerciseOwnerClientSavedSubject(t *testing.T, migration bool) {
	t.Helper()
	f := newOwnerContactFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	expected := ownerPreparationSubjectFixture(ownerRecoverSaved)
	if migration {
		expected = ownerPreparationSubjectFixture(ownerMigrateReferenced)
		expected.Migration.RootID = f.journal.pin.RootID()
	} else {
		expected.Source.RootID = f.journal.pin.RootID()
	}
	join := f.serve(t, func(connection *net.UnixConn, request ownerRequest) error {
		if request.Action != expected.Action || request.Source != expected.Source || request.Migration != expected.Migration || request.Context != expected.Context {
			return ErrIdentity
		}
		return writeOwnerFrame(connection, ownerServerReply(request, ownerIncompatible))
	})
	outcome, err := RequestBoundPreparation(ctx, f.reader, f.auth.request.Owner, PreparationRequest{Operation: expected.preparationOperation(), Context: expected.Context, Requirements: warmOwnerRequirements(f.auth.intent.Inputs.Artifacts)})
	if outcome.Disposition != (Disposition{}) || outcome.Condition != PreparationIncompatible || err != nil {
		t.Fatalf("subject contact outcome changed: outcome=%v err=%v", outcome, err)
	}
	if err = join(); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerClientPreservesOtherRootSelectionWithoutBirth(t *testing.T) {
	f := newOwnerContactFixture(t)
	targetJournal, _ := journalFixture(t)
	target, err := NewOrigin(targetJournal.pin.Root(), targetJournal.pin.RootID())
	if err != nil {
		t.Fatal(err)
	}
	selected, err := StageLaunchContext(t.Context(), targetJournal.store, contextForJournal(t, targetJournal))
	if err != nil {
		t.Fatal(err)
	}
	requirements := ownerRequirementsFixture()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	join := f.serve(t, func(connection *net.UnixConn, request ownerRequest) error {
		if request.Action != ownerEnsureOther || request.Target != (originRecord{Root: target.Root(), RootID: target.RootID()}) || request.Context != selected || request.Requirements != requirements {
			return ErrIdentity
		}
		return writeOwnerFrame(connection, ownerServerReply(request, ownerBusy))
	})
	established, err := RequestOtherEstablishment(ctx, f.reader, f.auth.request.Owner, target, selected, requirements, InitialOperation{})
	if established != (EstablishedOwner{}) || !errors.Is(err, ErrOwnerBusy) {
		t.Fatalf("other-root contact: owner=%v err=%v", established, err)
	}
	if err = join(); err != nil {
		t.Fatal(err)
	}
	if _, found, readErr := targetJournal.latestIntent(t.Context()); found || readErr != nil {
		t.Fatalf("request client created target startup: found=%t err=%v", found, readErr)
	}
}

func TestOwnerClientPreservesMigrationResultAlongsidePreparationFault(t *testing.T) {
	for _, code := range []ownerReplyCode{ownerOK, ownerFault, ownerTimeout} {
		t.Run(string(code), func(t *testing.T) {
			f := newOwnerContactFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			migration := MigrationReference{RootID: f.journal.pin.RootID(), RecordSHA256: ownerWireDigest, HistoryIndex: 2}
			selected := LaunchContextReference{SHA256: ownerWireDigest}
			join := f.serve(t, func(connection *net.UnixConn, request ownerRequest) error {
				reply := ownerServerReply(request, code)
				reply.Disposition = Disposition{Kind: DispositionStarted, Context: selected}
				reply.MigrationResult = MigrationCompleted
				reply.Target = originRecord{Root: f.journal.pin.Root(), RootID: request.RootID}
				reply.Startup = request.Owner
				return writeOwnerFrame(connection, reply)
			})
			outcome, err := RequestBoundPreparation(ctx, f.reader, f.auth.request.Owner, PreparationRequest{Operation: InitialOperation{Kind: InitialMigrateReferenced, Migration: migration}, Context: selected, Requirements: warmOwnerRequirements(f.auth.intent.Inputs.Artifacts)})
			if outcome.MigrationResult != MigrationCompleted || outcome.Disposition != (Disposition{Kind: DispositionStarted, Context: selected}) || outcome.Condition != PreparationCondition(code) || err != nil {
				t.Fatalf("migration outcome changed: outcome=%v err=%v", outcome, err)
			}
			if joinErr := join(); joinErr != nil {
				t.Fatal(joinErr)
			}
		})
	}
}
