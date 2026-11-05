package infrastructure

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func epochLifecycleIntentFixture(t *testing.T, kind epochLifecycleKind) epochLifecycleIntent {
	t.Helper()
	startup := startupIntentFixture(t)
	owner, err := ReferenceStartup(startup)
	if err != nil {
		t.Fatal(err)
	}
	intent := epochLifecycleIntent{
		Protocol: ProtocolVersion, Sequence: 3, ID: strings.Repeat("e", 32), Kind: kind,
		Root: startup.Inputs.Root, RootID: startup.Inputs.RootID, BootID: startup.Inputs.BootID,
		RootDevice: startup.Inputs.RootDevice, RootInode: startup.Inputs.RootInode, Owner: owner,
		AcceptedAt: "2026-11-03T00:00:00Z", Deadline: "2026-11-03T00:00:30Z",
	}
	if kind == epochLifecycleOwnerUpgrade {
		intent.SuccessorArtifacts, intent.SuccessorContext = startup.Inputs.Artifacts, startup.Inputs.Context
	}
	return intent
}

func epochLifecycleSuccessorFixture() StartupReference {
	return StartupReference{Sequence: 4, Epoch: strings.Repeat("f", 32), IntentSHA256: strings.Repeat("f", 64)}
}

func epochLifecycleHistoryFixture(t *testing.T, intent epochLifecycleIntent, phases ...epochLifecyclePhase) []epochLifecycleProgress {
	t.Helper()
	reference, err := referenceEpochLifecycle(intent)
	if err != nil {
		t.Fatal(err)
	}
	records := make([]epochLifecycleProgress, 0, len(phases))
	for _, phase := range phases {
		record := epochLifecycleProgress{Protocol: ProtocolVersion, OperationRef: reference, Phase: phase, RecordedAt: intent.AcceptedAt}
		if phase == epochLifecycleStopping {
			record.StopTarget = epochStopTarget{Kind: epochStopAbsent, BootID: intent.BootID, Directory: epochNativeFileIdentity{Device: 23, Inode: 41}, Bootstrap: epochNativeFileIdentity{Device: 23, Inode: 42}}
		}
		if phase == epochLifecycleComplete && intent.Kind == epochLifecycleOwnerUpgrade {
			record.Successor = epochLifecycleSuccessorFixture()
		}
		records = append(records, record)
	}
	return records
}

func TestEpochLifecycleCanonicalIntentAndDigest(t *testing.T) {
	intent := epochLifecycleIntentFixture(t, epochLifecycleStop)
	intent.Owner = StartupReference{}
	rootJSON, err := json.Marshal(intent.Root)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON := fmt.Sprintf(`{"protocol":1,"sequence":3,"id":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","kind":"stop","root":%s,"root_id":"0123456789abcdef0123456789abcdef","boot_id":"linux:11111111-2222-3333-4444-555555555555","root_device":23,"root_inode":41,"owner":{"sequence":0,"epoch":"","intent_sha256":""},"accepted_at":"2026-11-03T00:00:00Z","deadline":"2026-11-03T00:00:30Z","successor_artifacts":{"runner":"","runner_sha256":"","client":"","client_sha256":"","daemon":"","daemon_sha256":""},"successor_context":{"sha256":""}}`+"\n", rootJSON)
	data, err := task.MarshalCanonical(intent)
	if err != nil || string(data) != wantJSON {
		t.Fatalf("lifecycle canonical JSON changed: %v", err)
	}
	wantDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(wantJSON)))
	digest, err := intent.Digest()
	if err != nil || digest != wantDigest {
		t.Fatalf("intent digest = %q, %v; want independent SHA-256 %q", digest, err, wantDigest)
	}
	reference, err := referenceEpochLifecycle(intent)
	if err != nil || reference != (epochLifecycleReference{ID: intent.ID, SHA256: wantDigest}) {
		t.Fatalf("intent reference = %+v, %v", reference, err)
	}
	var decoded epochLifecycleIntent
	if err = task.DecodeStrict([]byte(wantJSON), &decoded); err != nil || decoded != intent {
		t.Fatalf("canonical lifecycle fixture round trip = %v", err)
	}
}

func TestEpochLifecycleIntentRejectsInvalidIdentityAndTime(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*epochLifecycleIntent)
	}{
		{"protocol", func(i *epochLifecycleIntent) { i.Protocol++ }},
		{"sequence", func(i *epochLifecycleIntent) { i.Sequence = 0 }},
		{"id", func(i *epochLifecycleIntent) { i.ID = strings.Repeat("A", 32) }},
		{"kind", func(i *epochLifecycleIntent) { i.Kind = "unknown" }},
		{"root path", func(i *epochLifecycleIntent) { i.Root = "relative" }},
		{"root id", func(i *epochLifecycleIntent) { i.RootID = "invalid" }},
		{"boot", func(i *epochLifecycleIntent) { i.BootID = "unknown" }},
		{"inode", func(i *epochLifecycleIntent) { i.RootInode = 0 }},
		{"owner sequence", func(i *epochLifecycleIntent) { i.Owner.Sequence = 0 }},
		{"owner epoch", func(i *epochLifecycleIntent) { i.Owner.Epoch = "invalid" }},
		{"owner hash", func(i *epochLifecycleIntent) { i.Owner.IntentSHA256 = "invalid" }},
		{"accepted missing", func(i *epochLifecycleIntent) { i.AcceptedAt = "" }},
		{"accepted zone", func(i *epochLifecycleIntent) { i.AcceptedAt = "2026-11-03T01:00:00+01:00" }},
		{"deadline zone", func(i *epochLifecycleIntent) { i.Deadline = "2026-11-03T01:00:30+01:00" }},
		{"deadline missing", func(i *epochLifecycleIntent) { i.Deadline = "" }},
		{"deadline equal", func(i *epochLifecycleIntent) { i.Deadline = i.AcceptedAt }},
		{"deadline before", func(i *epochLifecycleIntent) { i.Deadline = "2026-11-02T23:59:59Z" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			intent := epochLifecycleIntentFixture(t, epochLifecycleStop)
			test.mutate(&intent)
			if err := intent.Validate(); !errors.Is(err, ErrIdentity) {
				t.Fatalf("invalid lifecycle body accepted: %v", err)
			}
			if digest, err := intent.Digest(); digest != "" || !errors.Is(err, ErrIdentity) {
				t.Fatalf("invalid lifecycle body produced digest %q: %v", digest, err)
			}
		})
	}
}

func TestEpochLifecycleSuccessorHasOneArtifactAuthority(t *testing.T) {
	for _, test := range []struct {
		name   string
		kind   epochLifecycleKind
		mutate func(*epochLifecycleIntent)
	}{
		{"stop artifacts", epochLifecycleStop, func(i *epochLifecycleIntent) { i.SuccessorArtifacts.Runner = filepath.Join(i.Root, "runner") }},
		{"stop context", epochLifecycleStop, func(i *epochLifecycleIntent) { i.SuccessorContext.SHA256 = strings.Repeat("a", 64) }},
		{"upgrade no artifacts", epochLifecycleOwnerUpgrade, func(i *epochLifecycleIntent) { i.SuccessorArtifacts = Artifacts{} }},
		{"upgrade no context", epochLifecycleOwnerUpgrade, func(i *epochLifecycleIntent) { i.SuccessorContext = LaunchContextReference{} }},
		{"runner path", epochLifecycleOwnerUpgrade, func(i *epochLifecycleIntent) { i.SuccessorArtifacts.Runner = "relative" }},
		{"client path", epochLifecycleOwnerUpgrade, func(i *epochLifecycleIntent) { i.SuccessorArtifacts.Client = "relative" }},
		{"daemon path", epochLifecycleOwnerUpgrade, func(i *epochLifecycleIntent) { i.SuccessorArtifacts.Daemon = "relative" }},
		{"runner hash", epochLifecycleOwnerUpgrade, func(i *epochLifecycleIntent) { i.SuccessorArtifacts.RunnerSHA256 = "invalid" }},
		{"client hash", epochLifecycleOwnerUpgrade, func(i *epochLifecycleIntent) { i.SuccessorArtifacts.ClientSHA256 = "invalid" }},
		{"daemon hash", epochLifecycleOwnerUpgrade, func(i *epochLifecycleIntent) { i.SuccessorArtifacts.DaemonSHA256 = "invalid" }},
		{"context hash", epochLifecycleOwnerUpgrade, func(i *epochLifecycleIntent) { i.SuccessorContext.SHA256 = "invalid" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			intent := epochLifecycleIntentFixture(t, test.kind)
			test.mutate(&intent)
			if err := intent.Validate(); !errors.Is(err, ErrIdentity) {
				t.Fatalf("invalid successor selection accepted: %v", err)
			}
		})
	}
}

func TestEpochLifecycleReferenceBindsEveryBodyIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*epochLifecycleIntent)
	}{
		{"sequence", func(i *epochLifecycleIntent) { i.Sequence++ }},
		{"id", func(i *epochLifecycleIntent) { i.ID = strings.Repeat("b", 32) }},
		{"root", func(i *epochLifecycleIntent) { i.Root = filepath.Join(i.Root, "replacement") }},
		{"root id", func(i *epochLifecycleIntent) { i.RootID = strings.Repeat("b", 32) }},
		{"boot", func(i *epochLifecycleIntent) { i.BootID = "linux:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" }},
		{"device", func(i *epochLifecycleIntent) { i.RootDevice++ }},
		{"inode", func(i *epochLifecycleIntent) { i.RootInode++ }},
		{"owner", func(i *epochLifecycleIntent) { i.Owner = epochLifecycleSuccessorFixture() }},
		{"accepted", func(i *epochLifecycleIntent) { i.AcceptedAt = "2026-11-03T00:00:01Z" }},
		{"deadline", func(i *epochLifecycleIntent) { i.Deadline = "2026-11-03T00:00:31Z" }},
		{"artifact", func(i *epochLifecycleIntent) { i.SuccessorArtifacts.RunnerSHA256 = strings.Repeat("b", 64) }},
		{"context", func(i *epochLifecycleIntent) { i.SuccessorContext.SHA256 = strings.Repeat("b", 64) }},
		{"kind", func(i *epochLifecycleIntent) {
			i.Kind = epochLifecycleStop
			i.SuccessorArtifacts = Artifacts{}
			i.SuccessorContext = LaunchContextReference{}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			intent := epochLifecycleIntentFixture(t, epochLifecycleOwnerUpgrade)
			reference, err := referenceEpochLifecycle(intent)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&intent)
			if err = intent.Validate(); err != nil {
				t.Fatalf("changed body fixture is invalid: %v", err)
			}
			if err = reference.validateIntent(intent); !errors.Is(err, ErrIdentity) {
				t.Fatalf("body changed under original reference: %v", err)
			}
		})
	}
}

func TestEpochLifecycleDerivesLegalHistoriesWithoutMutation(t *testing.T) {
	for _, kind := range []epochLifecycleKind{epochLifecycleStop, epochLifecycleOwnerUpgrade} {
		dispatched := epochLifecycleStopping
		if kind == epochLifecycleOwnerUpgrade {
			dispatched = epochLifecycleExiting
		}
		for _, phases := range [][]epochLifecyclePhase{
			{},
			{epochLifecycleClosing},
			{epochLifecycleClosing, dispatched},
			{epochLifecycleClosing, dispatched, epochLifecycleComplete},
			{epochLifecycleClosing, epochLifecycleAborted},
		} {
			t.Run(fmt.Sprint(kind, phases), func(t *testing.T) {
				intent := epochLifecycleIntentFixture(t, kind)
				records := epochLifecycleHistoryFixture(t, intent, phases...)
				want := epochLifecycleAccepted
				if len(phases) != 0 {
					want = phases[len(phases)-1]
				}
				requireEpochLifecyclePhase(t, intent, records, want)
			})
		}
	}
}

func requireEpochLifecyclePhase(t *testing.T, intent epochLifecycleIntent, records []epochLifecycleProgress, want epochLifecyclePhase) {
	t.Helper()
	beforeIntent, beforeRecords := intent, slices.Clone(records)
	phase, err := deriveEpochLifecyclePhase(intent, records)
	if err != nil || phase != want {
		t.Fatalf("history phase = %s, %v; want %s", phase, err, want)
	}
	if intent != beforeIntent || !slices.Equal(records, beforeRecords) {
		t.Fatal("phase derivation mutated immutable input")
	}
}

func TestEpochLifecycleExplicitAbandonmentIsTerminal(t *testing.T) {
	// This is the immutable history shape only. The serialized writer must
	// separately prove explicit replacement, absence and settlement of every
	// successor, then fence continuation before recording this terminal.
	const abandoned epochLifecyclePhase = "abandoned"
	intent := epochLifecycleIntentFixture(t, epochLifecycleOwnerUpgrade)
	history := epochLifecycleHistoryFixture(t, intent, epochLifecycleClosing, epochLifecycleExiting, abandoned)
	history[len(history)-1].RecordedAt = "2026-11-03T00:01:00Z"
	requireEpochLifecyclePhase(t, intent, history, abandoned)
	for _, next := range []epochLifecyclePhase{epochLifecycleClosing, epochLifecycleExiting, epochLifecycleComplete, epochLifecycleAborted, abandoned} {
		reopened := append(slices.Clone(history), epochLifecycleHistoryFixture(t, intent, next)...)
		if _, err := deriveEpochLifecyclePhase(intent, reopened); !errors.Is(err, ErrIdentity) {
			t.Fatalf("abandoned upgrade reopened as %s: %v", next, err)
		}
	}
	withSuccessor := slices.Clone(history)
	withSuccessor[len(withSuccessor)-1].Successor = epochLifecycleSuccessorFixture()
	if _, err := deriveEpochLifecyclePhase(intent, withSuccessor); !errors.Is(err, ErrIdentity) {
		t.Fatalf("abandonment claimed a successor: %v", err)
	}
	for _, kind := range []epochLifecycleKind{epochLifecycleStop, epochLifecycleOwnerUpgrade} {
		body := epochLifecycleIntentFixture(t, kind)
		for _, phases := range [][]epochLifecyclePhase{{abandoned}, {epochLifecycleClosing, abandoned}, {epochLifecycleClosing, epochLifecycleStopping, abandoned}} {
			if _, err := deriveEpochLifecyclePhase(body, epochLifecycleHistoryFixture(t, body, phases...)); !errors.Is(err, ErrIdentity) {
				t.Fatalf("invalid %s abandonment history %v accepted: %v", kind, phases, err)
			}
		}
	}
}

func TestEpochLifecycleRejectsIllegalHistories(t *testing.T) {
	for _, kind := range []epochLifecycleKind{epochLifecycleStop, epochLifecycleOwnerUpgrade} {
		dispatched, wrongDispatch := epochLifecycleStopping, epochLifecycleExiting
		if kind == epochLifecycleOwnerUpgrade {
			dispatched, wrongDispatch = wrongDispatch, dispatched
		}
		for _, phases := range [][]epochLifecyclePhase{
			{dispatched},
			{epochLifecycleComplete},
			{epochLifecycleAborted},
			{epochLifecycleAccepted},
			{epochLifecycleClosing, epochLifecycleClosing},
			{epochLifecycleClosing, epochLifecycleComplete},
			{epochLifecycleClosing, wrongDispatch},
			{epochLifecycleClosing, dispatched, dispatched},
			{epochLifecycleClosing, dispatched, epochLifecycleClosing},
			{epochLifecycleClosing, dispatched, epochLifecycleAborted},
			{epochLifecycleClosing, dispatched, epochLifecycleComplete, epochLifecycleAborted},
			{epochLifecycleClosing, dispatched, epochLifecycleComplete, epochLifecycleComplete},
			{epochLifecycleClosing, epochLifecycleAborted, epochLifecycleComplete},
			{epochLifecycleClosing, epochLifecycleAborted, epochLifecycleClosing},
			{epochLifecycleClosing, epochLifecycleAborted, epochLifecycleAborted},
			{epochLifecycleClosing, "unknown"},
		} {
			t.Run(fmt.Sprint(kind, phases), func(t *testing.T) {
				intent := epochLifecycleIntentFixture(t, kind)
				records := epochLifecycleHistoryFixture(t, intent, phases...)
				if phase, err := deriveEpochLifecyclePhase(intent, records); phase != "" || !errors.Is(err, ErrIdentity) {
					t.Fatalf("invalid history became phase %s: %v", phase, err)
				}
			})
		}
	}
}

func TestEpochLifecycleProgressRejectsWrongSubjectTimeAndSuccessor(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*epochLifecycleProgress)
	}{
		{"protocol", func(p *epochLifecycleProgress) { p.Protocol++ }},
		{"operation id shape", func(p *epochLifecycleProgress) { p.OperationRef.ID = "invalid" }},
		{"operation id subject", func(p *epochLifecycleProgress) { p.OperationRef.ID = strings.Repeat("b", 32) }},
		{"operation hash shape", func(p *epochLifecycleProgress) { p.OperationRef.SHA256 = "invalid" }},
		{"operation hash subject", func(p *epochLifecycleProgress) { p.OperationRef.SHA256 = strings.Repeat("b", 64) }},
		{"recorded missing", func(p *epochLifecycleProgress) { p.RecordedAt = "" }},
		{"recorded zone", func(p *epochLifecycleProgress) { p.RecordedAt = "2026-11-03T01:00:00+01:00" }},
		{"recorded before acceptance", func(p *epochLifecycleProgress) { p.RecordedAt = "2026-11-02T23:59:59Z" }},
		{"unknown phase", func(p *epochLifecycleProgress) { p.Phase = "unknown" }},
		{"accepted phase", func(p *epochLifecycleProgress) { p.Phase = epochLifecycleAccepted }},
		{"stop exiting", func(p *epochLifecycleProgress) { p.Phase = epochLifecycleExiting }},
		{"closing successor", func(p *epochLifecycleProgress) { p.Successor = epochLifecycleSuccessorFixture() }},
		{"stop complete successor", func(p *epochLifecycleProgress) {
			p.Phase = epochLifecycleComplete
			p.Successor = epochLifecycleSuccessorFixture()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			intent := epochLifecycleIntentFixture(t, epochLifecycleStop)
			record := epochLifecycleHistoryFixture(t, intent, epochLifecycleClosing)[0]
			test.mutate(&record)
			if err := record.Validate(intent); !errors.Is(err, ErrIdentity) {
				t.Fatalf("invalid lifecycle progress accepted: %v", err)
			}
		})
	}
}

func TestEpochLifecycleUpgradeCompletionRequiresSuccessorShapeOnly(t *testing.T) {
	intent := epochLifecycleIntentFixture(t, epochLifecycleOwnerUpgrade)
	complete := epochLifecycleHistoryFixture(t, intent, epochLifecycleComplete)[0]
	if err := complete.Validate(intent); err != nil {
		t.Fatalf("shape-only successor requires unavailable native evidence: %v", err)
	}
	for _, successor := range []StartupReference{
		{},
		{Sequence: 4},
		{Sequence: 4, Epoch: strings.Repeat("f", 32)},
		{Sequence: 4, Epoch: "invalid", IntentSHA256: strings.Repeat("f", 64)},
		{Sequence: 0, Epoch: strings.Repeat("f", 32), IntentSHA256: strings.Repeat("f", 64)},
	} {
		invalid := complete
		invalid.Successor = successor
		if err := invalid.Validate(intent); !errors.Is(err, ErrIdentity) {
			t.Fatalf("invalid upgrade successor accepted: %+v, %v", successor, err)
		}
	}
	closing := epochLifecycleHistoryFixture(t, intent, epochLifecycleClosing)[0]
	closing.Successor = complete.Successor
	if err := closing.Validate(intent); !errors.Is(err, ErrIdentity) {
		t.Fatalf("upgrade chose successor before completion: %v", err)
	}
}

func TestEpochLifecycleLateReconciliationPreservesOriginalBody(t *testing.T) {
	for _, terminal := range []epochLifecyclePhase{epochLifecycleComplete, epochLifecycleAborted} {
		t.Run(string(terminal), func(t *testing.T) {
			intent := epochLifecycleIntentFixture(t, epochLifecycleOwnerUpgrade)
			before, err := task.MarshalCanonical(intent)
			if err != nil {
				t.Fatal(err)
			}
			phases := []epochLifecyclePhase{epochLifecycleClosing, epochLifecycleExiting, terminal}
			if terminal == epochLifecycleAborted {
				phases = []epochLifecyclePhase{epochLifecycleClosing, terminal}
			}
			records := epochLifecycleHistoryFixture(t, intent, phases...)
			records[len(records)-1].RecordedAt = "2026-11-04T00:00:00Z"
			if phase, deriveErr := deriveEpochLifecyclePhase(intent, records); deriveErr != nil || phase != terminal {
				t.Fatalf("late reconciliation = %s, %v", phase, deriveErr)
			}
			after, err := task.MarshalCanonical(intent)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("late reconciliation rewrote lifecycle body: %v", err)
			}
			if phase, deriveErr := deriveEpochLifecyclePhase(intent, nil); deriveErr != nil || phase != epochLifecycleAccepted {
				t.Fatalf("missing progress implicitly completed expired intent: %s, %v", phase, deriveErr)
			}
		})
	}
}

func TestEpochLifecycleOwnerAbsenceAndSuccessorDeadlineAreSeparateProofs(t *testing.T) {
	for _, kind := range []epochLifecycleKind{epochLifecycleStop, epochLifecycleOwnerUpgrade} {
		intent := epochLifecycleIntentFixture(t, kind)
		intent.Owner = StartupReference{}
		if err := intent.Validate(); err != nil {
			t.Fatal(err)
		}
		if phase, err := deriveEpochLifecyclePhase(intent, nil); err != nil || phase != epochLifecycleAccepted {
			t.Fatalf("zero owner shape fabricated completion: %s, %v", phase, err)
		}
	}
	intent := epochLifecycleIntentFixture(t, epochLifecycleOwnerUpgrade)
	reference, err := referenceEpochLifecycle(intent)
	if err != nil {
		t.Fatal(err)
	}
	successor := startupIntentFixture(t)
	successor.Sequence, successor.Epoch = 4, strings.Repeat("f", 32)
	successor.Inputs = StartupInputs{
		Kind: StartupUpgrade, Root: intent.Root, RootID: intent.RootID, BootID: intent.BootID,
		RootDevice: intent.RootDevice, RootInode: intent.RootInode, UpgradeID: reference.ID, UpgradeSHA256: reference.SHA256,
	}
	successor.Starter.Root, successor.Starter.RootID = intent.Root, intent.RootID
	successor.AcceptedAt, successor.Deadline = "2026-11-04T00:00:00Z", "2026-11-04T00:00:30Z"
	successor.InputsSHA256, err = successor.Inputs.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err = successor.Validate(); err != nil {
		t.Fatalf("successor could not have its own finite attempt deadline: %v", err)
	}
	if successor.Inputs.Artifacts != (Artifacts{}) || successor.Inputs.Context != (LaunchContextReference{}) {
		t.Fatal("successor introduced another artifact authority")
	}
	if err = reference.validateIntent(intent); err != nil || intent.Deadline != "2026-11-03T00:00:30Z" {
		t.Fatalf("successor changed historical lifecycle deadline: %v", err)
	}
}

func TestEpochLifecycleStrictDecodingAndRecordSize(t *testing.T) {
	intent := epochLifecycleIntentFixture(t, epochLifecycleStop)
	data, err := task.MarshalCanonical(intent)
	if err != nil {
		t.Fatal(err)
	}
	for _, malformed := range [][]byte{
		append(bytes.Clone(data), []byte("{}")...),
		bytes.Replace(data, []byte(`"protocol":1`), []byte(`"protocol":1,"protocol":1`), 1),
		bytes.Replace(data, []byte(`"protocol":1`), []byte(`"unknown":1`), 1),
	} {
		var decoded epochLifecycleIntent
		if decodeErr := task.DecodeStrict(malformed, &decoded); decodeErr == nil {
			t.Fatal("malformed lifecycle JSON accepted")
		}
	}
	intent = epochLifecycleIntentFixture(t, epochLifecycleOwnerUpgrade)
	intent.SuccessorArtifacts.Runner = string(filepath.Separator) + strings.Repeat("x", task.MaxControlRecordSize)
	if _, err = intent.Digest(); !errors.Is(err, task.ErrControlRecordTooBig) {
		t.Fatalf("oversized lifecycle body accepted: %v", err)
	}
}

func TestEpochLifecycleStopTargetPayloadBoundary(t *testing.T) {
	body := epochLifecycleIntentFixture(t, epochLifecycleStop)
	captured := epochStopTarget{Kind: epochStopCaptured, BootID: body.BootID, Directory: epochNativeFileIdentity{Inode: 41}, Bootstrap: epochNativeFileIdentity{Inode: 42}, PID: 7, StartToken: "darwin-v2:opaque-pueue-token", Binding: task.SupervisorRef{ConfigPath: filepath.Join(body.Root, ".supervisor", "pueue.yml"), ConfigDigest: strings.Repeat("a", 64), Endpoint: filepath.Join(body.Root, ".supervisor", "pueue.sock"), ObservedVersion: "4.0.4"}}
	if err := captured.Validate(); err != nil {
		t.Fatalf("opaque control-owned token was parsed as lifetime identity: %v", err)
	}
	absent := captured
	absent.Kind = epochStopAbsent
	absent.PID = 0
	absent.StartToken = ""
	if err := absent.Validate(); err != nil {
		t.Fatal(err)
	}
	neverStarted := absent
	neverStarted.Binding = task.SupervisorRef{}
	if err := neverStarted.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*epochStopTarget){
		func(target *epochStopTarget) { target.Kind = "unknown" },
		func(target *epochStopTarget) { target.BootID = "unknown" },
		func(target *epochStopTarget) { target.Directory.Inode = 0 },
		func(target *epochStopTarget) { target.Bootstrap.Inode = 0 },
		func(target *epochStopTarget) { target.Binding = task.SupervisorRef{} },
		func(target *epochStopTarget) { target.PID = 0 },
		func(target *epochStopTarget) { target.StartToken = "" },
		func(target *epochStopTarget) { target.StartToken = "bad\x00token" },
		func(target *epochStopTarget) { target.Kind = epochStopAbsent },
	} {
		changed := captured
		mutate(&changed)
		if err := changed.Validate(); err == nil {
			t.Fatal("malformed stopping payload accepted")
		}
	}
	record := epochLifecycleHistoryFixture(t, body, epochLifecycleStopping)[0]
	record.StopTarget = captured
	if err := record.Validate(body); err != nil {
		t.Fatal(err)
	}
	record.Phase = epochLifecycleClosing
	if err := record.Validate(body); err == nil {
		t.Fatal("Stop capture escaped Stopping record")
	}
}
