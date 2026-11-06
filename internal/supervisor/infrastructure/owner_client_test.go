package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOwnerClientRejectsLivePreviousCodecWithoutRewritingRecords(t *testing.T) {
	f := newOwnerContactFixture(t)
	if f.auth.publication.Protocol != 3 || ownerProtocolVersion != 4 || ownerStopProtocolVersion != 1 {
		t.Fatal("owner wire, record and stop protocols were conflated")
	}
	join := f.serve(t, func(connection *net.UnixConn, request ownerRequest) error {
		reply := ownerServerReply(request, ownerOK)
		reply.Protocol = 3
		data, err := json.Marshal(reply)
		if err != nil {
			return err
		}
		previous := strings.TrimSuffix(string(data), "}") + `,"migration":{"root_id":"","record_sha256":"","history_index":0}}` + "\n"
		_, err = connection.Write([]byte(previous))
		return err
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	owner, err := ProbeOwner(ctx, f.reader)
	if !errors.Is(err, ErrOwnerIncompatible) || owner != (StartupReference{}) {
		t.Fatalf("live previous-codec owner was accepted: %v", err)
	}
	if err := join(); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerClientPreservesManualReportAlongsidePreparationFault(t *testing.T) {
	for _, code := range []ownerReplyCode{ownerOK, ownerFault, ownerTimeout} {
		t.Run(string(code), func(t *testing.T) {
			f := newOwnerContactFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			selected := LaunchContextReference{SHA256: ownerWireDigest}
			report := ManualMigrationReport{State: ManualMigrationMigrated, Legacy: ManualMigrationLegacyRecovered, Active: ManualMigrationActiveNone}
			join := f.serve(t, func(connection *net.UnixConn, request ownerRequest) error {
				reply := ownerServerReply(request, code)
				reply.Disposition = Disposition{Kind: DispositionStarted, Context: selected}
				reply.Report = report
				reply.Target = originRecord{Root: f.journal.pin.Root(), RootID: request.RootID}
				reply.Startup = request.Owner
				return writeOwnerFrame(connection, reply)
			})
			outcome, err := RequestBoundPreparation(ctx, f.reader, f.auth.request.Owner, PreparationRequest{Operation: InitialOperation{Kind: InitialMigrateOwn}, Context: selected})
			if outcome.Report != report || outcome.Disposition != (Disposition{Kind: DispositionStarted, Context: selected}) || outcome.Condition != PreparationCondition(code) || err != nil {
				t.Fatalf("manual outcome changed: outcome=%v err=%v", outcome, err)
			}
			if joinErr := join(); joinErr != nil {
				t.Fatal(joinErr)
			}
		})
	}
}

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

func TestOwnerClientProbePreservesReferenceAndReplyCause(t *testing.T) {
	for _, row := range []struct {
		code  ownerReplyCode
		cause error
	}{
		{ownerPending, ErrOwnerPending},
		{ownerBusy, ErrOwnerBusy},
		{ownerIncompatible, ErrOwnerIncompatible},
		{ownerFault, ErrOwnerRequest},
		{ownerTimeout, context.DeadlineExceeded},
	} {
		t.Run(string(row.code), func(t *testing.T) {
			f := newOwnerContactFixture(t)
			join := f.serve(t, func(connection *net.UnixConn, request ownerRequest) error {
				return writeOwnerFrame(connection, ownerServerReply(request, row.code))
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			reference, err := ProbeOwner(ctx, f.reader)
			if reference != f.auth.request.Owner || !errors.Is(err, row.cause) {
				t.Fatalf("probe lost reference or reply cause: reference=%v err=%v", reference, err)
			}
			if row.code == ownerTimeout && !errors.Is(err, ErrOwnerRequest) {
				t.Fatal("owner timeout lost request-failure cause")
			}
			if err = join(); err != nil {
				t.Fatal(err)
			}
		})
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
	outcome, err := RequestBoundPreparation(ctx, f.reader, f.auth.request.Owner, PreparationRequest{Context: reference})
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
	outcome, err := RequestBoundPreparation(ctx, f.reader, f.auth.request.Owner, PreparationRequest{})
	if unknown {
		if outcome != (PreparationOutcome{}) || err == nil {
			t.Fatalf("unknown contact became success: outcome=%v err=%v", outcome, err)
		}
		requireOwnerContactError(t, err)
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

func TestOwnerClientPreservesSavedSubject(t *testing.T) {
	t.Helper()
	f := newOwnerContactFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	expected := ownerPreparationSubjectFixture(ownerRecoverSaved)
	expected.Source.RootID = f.journal.pin.RootID()
	join := f.serve(t, func(connection *net.UnixConn, request ownerRequest) error {
		if request.Action != expected.Action || request.Source != expected.Source || request.Context != expected.Context {
			return ErrIdentity
		}
		return writeOwnerFrame(connection, ownerServerReply(request, ownerIncompatible))
	})
	outcome, err := RequestBoundPreparation(ctx, f.reader, f.auth.request.Owner, PreparationRequest{Operation: expected.preparationOperation(), Context: expected.Context})
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

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	join := f.serve(t, func(connection *net.UnixConn, request ownerRequest) error {
		if request.Action != ownerEnsureOther || request.Target != (originRecord{Root: target.Root(), RootID: target.RootID()}) || request.Context != selected {
			return ErrIdentity
		}
		return writeOwnerFrame(connection, ownerServerReply(request, ownerBusy))
	})
	established, err := RequestOtherEstablishment(ctx, f.reader, f.auth.request.Owner, target, selected, InitialOperation{})
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
