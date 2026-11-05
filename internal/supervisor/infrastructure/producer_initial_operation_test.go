package infrastructure

import (
	"errors"
	"maps"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func seedProducerInitialIntent(t *testing.T, f producerDataFixture, initial InitialOperation) StartupIntent {
	t.Helper()
	intent := journalIntent(t, f.journal, 1)
	intent.Inputs.Artifacts = f.host.artifacts
	reference, _, err := f.journal.stageContext(t.Context(), f.request.Context)
	if err != nil {
		t.Fatal(err)
	}
	intent.Inputs.Context, intent.Inputs.Initial = reference, initial
	intent.Protocol = startupInitialOperationProtocolVersion
	intent.AcceptedAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	intent.Deadline = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	refreshIntentDigest(t, &intent)
	requireJournalPut(t, f.journal, f.lease, intent)
	return intent
}

func TestProducerInitialOperationAcceptance(t *testing.T) {
	for index := range initialOperationFixtures(t, originTestRootID) {
		f := producerFixture(t)
		operation := initialOperationFixtures(t, f.journal.pin.RootID())[index]
		releaseProducerFixtureLease(t, f)
		request := f.request
		request.Initial = operation
		refusal := errors.New("refuse before native Start")
		starts := 0
		attempt, err := establishOutside(t.Context(), f.journal.store, request, f.host, producerNative{start: func(*exec.Cmd) error {
			starts++
			return refusal
		}})
		ownProducerAttempt(t, attempt)
		if !errors.Is(err, refusal) || attempt == nil || starts != 1 || f.host.calls != 1 {
			t.Fatalf("operation=%+v acceptance: starts=%d prepare=%d err=%v", operation, starts, f.host.calls, err)
		}
		stored, err := f.journal.readIntent(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		wantInitial := operation
		if operation.Kind == InitialEnsureOwn {
			wantInitial = InitialOperation{}
		}
		wantProtocol := 2
		if wantInitial == (InitialOperation{}) {
			wantProtocol = 1
		}
		if stored.Protocol != wantProtocol || stored.Inputs.Initial != wantInitial || request.Initial != operation {
			t.Fatalf("accepted operation changed: protocol=%d initial=%+v", stored.Protocol, stored.Inputs.Initial)
		}
		if stored.Deadline != request.Deadline.UTC().Format(time.RFC3339Nano) {
			t.Fatal("acceptance changed request deadline")
		}
	}
}

func TestProducerInitialOperationRefusedBeforeWrites(t *testing.T) {
	for _, name := range []string{"unknown", "wrong-root", "own-subject"} {
		t.Run(name, func(t *testing.T) {
			f := producerFixture(t)
			releaseProducerFixtureLease(t, f)
			before := producerBudgetTree(t, f.journal.pin.Root())
			request := f.request
			source, _ := savedBindingSourceFixture(t, SavedBindingTaskMetadata)
			source.RootID = f.journal.pin.RootID()
			switch name {
			case "unknown":
				request.Initial.Kind = "unknown"
			case "wrong-root":
				source.RootID = strings.Repeat("f", 32)
				request.Initial = InitialOperation{Kind: InitialRecoverSaved, Source: source}
			case "own-subject":
				request.Initial = InitialOperation{Kind: InitialEnsureOwn, Source: source}
			}
			attempt, err := EstablishOutside(t.Context(), f.journal.store, request, f.host)
			ownProducerAttempt(t, attempt)
			if !errors.Is(err, ErrIdentity) || attempt != nil || f.host.calls != 0 {
				t.Fatalf("invalid purpose accepted: attempt=%t prepare=%d err=%v", attempt != nil, f.host.calls, err)
			}
			if !maps.Equal(before, producerBudgetTree(t, f.journal.pin.Root())) {
				t.Fatal("invalid purpose changed target bytes, modes or modification times")
			}
		})
	}
}

func TestProducerPendingInitialOperationMatch(t *testing.T) {
	for _, free := range []bool{false, true} {
		for _, change := range []string{"same", "purpose", "subject", "artifacts-first"} {
			t.Run(change+"/free="+map[bool]string{false: "false", true: "true"}[free], func(t *testing.T) {
				checkProducerPendingInitialOperation(t, free, change)
			})
		}
	}
}

func checkProducerPendingInitialOperation(t *testing.T, free bool, change string) {
	t.Helper()
	f := producerFixture(t)
	source, _ := savedBindingSourceFixture(t, SavedBindingTaskStop)
	source.RootID = f.journal.pin.RootID()
	initial := InitialOperation{Kind: InitialRecoverSaved, Source: source}
	intent := seedProducerInitialIntent(t, f, initial)
	if free {
		releaseProducerFixtureLease(t, f)
	}
	request := f.request
	request.Initial, request.Deadline = initial, time.Now().Add(30*time.Second)
	switch change {
	case "purpose":
		request.Initial = InitialOperation{Kind: InitialMigrateOwn}
	case "subject":
		request.Initial.Source.RecordID = "budget"
	case "artifacts-first":
		request.Initial = InitialOperation{Kind: InitialMigrateOwn}
		request.Requirements.RequiredRuntimeSHA256 = strings.Repeat("f", 64)
	}
	attempt, err := EstablishOutside(t.Context(), f.journal.store, request, f.host)
	ownProducerAttempt(t, attempt)
	requireProducerPendingOutcome(t, change, attempt, err)
	if attempt != nil && attempt.intent != intent {
		t.Fatal("join changed accepted intent or original deadline")
	}
	stored, readErr := f.journal.readIntent(t.Context(), 1)
	if readErr != nil || stored != intent || f.host.calls != 0 {
		t.Fatalf("pending attempt changed: prepare=%d err=%v", f.host.calls, readErr)
	}
	if _, readErr = f.journal.readIntent(t.Context(), 2); !task.RecordAbsent(readErr) {
		t.Fatalf("join accepted a successor: %v", readErr)
	}
}

func requireProducerPendingOutcome(t *testing.T, change string, attempt *StartupAttempt, err error) {
	t.Helper()
	if change == "same" {
		if err != nil || attempt == nil || attempt.NativeCompletion() != nil {
			t.Fatalf("matching purpose did not join unchanged attempt: %v", err)
		}
		return
	}
	want := task.ErrLockBusy
	if change == "artifacts-first" {
		want = ErrIncompatible
	}
	if !errors.Is(err, want) || attempt != nil {
		t.Fatalf("mismatch outcome: attempt=%t err=%v want=%v", attempt != nil, err, want)
	}
}

func TestProducerExplicitOwnJoinsLegacyPending(t *testing.T) {
	f := producerFixture(t)
	intent := seedProducerIntent(t, f, 1, false)
	request := f.request
	request.Initial.Kind = InitialEnsureOwn
	attempt, err := EstablishOutside(t.Context(), f.journal.store, request, f.host)
	ownProducerAttempt(t, attempt)
	if err != nil || attempt == nil || attempt.intent != intent || f.host.calls != 0 {
		t.Fatalf("explicit Own did not join legacy acceptance: %v", err)
	}
}
