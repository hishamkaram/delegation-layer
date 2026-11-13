package infrastructure

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func runtimeReferenceFixture() RuntimeRestartReference {
	return RuntimeRestartReference{RootID: strings.Repeat("a", 32), Startup: StartupReference{Sequence: 7, Epoch: strings.Repeat("b", 32), IntentSHA256: strings.Repeat("c", 64)}}
}

func TestRuntimeRestartReferenceCanonicalToken(t *testing.T) {
	const token = "v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:7:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	expected := runtimeReferenceFixture()
	got, err := FormatRuntimeRestartReference(expected)
	if err != nil || got != token {
		t.Fatalf("canonical token changed: %q %v", got, err)
	}
	parsed, err := ParseRuntimeRestartReference(token)
	if err != nil || parsed != expected {
		t.Fatalf("canonical reference changed: %+v %v", parsed, err)
	}
	for _, value := range []string{
		"", token + ":extra", " " + token, strings.Replace(token, "v1:", "v2:", 1),
		strings.Replace(token, ":7:", ":07:", 1), strings.Replace(token, ":7:", ":+7:", 1),
		strings.Replace(token, ":7:", ":0:", 1), strings.Replace(token, ":7:", ":18446744073709551616:", 1),
		strings.Replace(token, strings.Repeat("b", 32), "foreign", 1), strings.Repeat("x", 155),
	} {
		if parsed, err = ParseRuntimeRestartReference(value); !errors.Is(err, ErrIdentity) || parsed != (RuntimeRestartReference{}) {
			t.Fatalf("noncanonical reference accepted: %+v %v", parsed, err)
		}
	}
}

func TestRuntimeRestartReferenceReadsExactPinnedIntent(t *testing.T) {
	j, lease := journalFixture(t)
	intent := journalIntent(t, j, 1)
	requireJournalPut(t, j, lease, intent)
	startup, err := ReferenceStartup(intent)
	if err != nil {
		t.Fatal(err)
	}
	reference := RuntimeRestartReference{RootID: j.pin.RootID(), Startup: startup}
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), j.pin.Root(), predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	before := producerBudgetTree(t, j.pin.Root())
	if err = ValidateRuntimeRestartReference(t.Context(), reader, reference); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []RuntimeRestartReference{
		{RootID: strings.Repeat("f", 32), Startup: startup},
		{RootID: reference.RootID, Startup: StartupReference{Sequence: startup.Sequence, Epoch: startup.Epoch, IntentSHA256: strings.Repeat("f", 64)}},
	} {
		if err = ValidateRuntimeRestartReference(t.Context(), reader, changed); !errors.Is(err, ErrIdentity) {
			t.Fatalf("foreign/changed startup accepted: %v", err)
		}
	}
	if !maps.Equal(before, producerBudgetTree(t, j.pin.Root())) {
		t.Fatal("passive reference validation changed root state")
	}
}

func TestRuntimeRestartPurposeAndBirthRejectOwnerProvenance(t *testing.T) {
	from := runtimeReferenceFixture().Startup
	initial := InitialOperation{Kind: InitialRuntimeRestart, RestartFrom: from}
	if err := initial.Validate(originTestRootID); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []InitialOperation{{Kind: InitialRuntimeRestart}, {Kind: InitialEnsureOwn, RestartFrom: from}, {Kind: InitialRuntimeRestart, RestartFrom: from, Source: SavedBindingSource{RootID: originTestRootID}}} {
		if err := invalid.Validate(originTestRootID); !errors.Is(err, ErrIdentity) {
			t.Fatalf("mixed/empty purpose accepted: %+v %v", invalid, err)
		}
	}
	intent := startupIntentFixture(t)
	intent.Inputs.Initial, intent.Inputs.RuntimeProducer = initial, producerOutside
	if err := intent.Inputs.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeRestartBirth(intent, []string{}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []producerKind{"", producerResident} {
		intent.Inputs.RuntimeProducer = kind
		if err := intent.Inputs.Validate(); !errors.Is(err, ErrIdentity) {
			t.Fatalf("non-outside accepted into startup: kind=%q err=%v", kind, err)
		}
		if err := validateRuntimeRestartBirth(intent, []string{}); !errors.Is(err, ErrOwnerRequest) {
			t.Fatalf("resident consumption accepted non-outside birth: %v", err)
		}
	}
	intent.Inputs.RuntimeProducer = producerOutside
	if err := validateRuntimeRestartBirth(intent, []string{OriginEnvironment + "=untrusted"}); !errors.Is(err, ErrOwnerRequest) {
		t.Fatalf("inherited origin was treated as outside: %v", err)
	}
}

func TestRuntimeRestartOwnerProducerRefusedBeforeRootEffects(t *testing.T) {
	f := producerFixture(t)
	request := f.request
	request.Initial = InitialOperation{Kind: InitialRuntimeRestart, RestartFrom: runtimeReferenceFixture().Startup}
	starts, prepares := 0, 0
	before := producerBudgetTree(t, f.journal.pin.Root())
	source := capturedEstablishment{
		starter: capturedProducerStarter{environment: []string{}, kind: producerResident},
		prepare: func(context.Context, *taskdir.Store) (string, error) { prepares++; return f.host.artifacts.Runner, nil },
	}
	attempt, err := establishCaptured(t.Context(), f.journal.store, request, source, producerNative{start: func(*exec.Cmd) error { starts++; return errors.New("unexpected Start") }})
	if attempt != nil || !errors.Is(err, ErrOwnerRequest) || starts != 0 || prepares != 0 {
		t.Fatalf("owner producer reached acceptance: attempt=%v starts=%d prepares=%d err=%v", attempt != nil, starts, prepares, err)
	}
	if !maps.Equal(before, producerBudgetTree(t, f.journal.pin.Root())) {
		t.Fatal("refused owner producer changed target state")
	}
	server := &ownerServer{}
	_, err = server.prepareOtherRoot(t.Context(), acceptedOwnerWork{request: ownerRequest{Initial: request.Initial}})
	if !errors.Is(err, ErrOwnerRequest) {
		t.Fatalf("other-root producer bypassed outside requirement: %v", err)
	}
}

func TestRuntimeRestartLegacyPublicationDigestAndCapability(t *testing.T) {
	f := newOwnerAuthFixture(t)
	legacy := f.publication
	legacy.RuntimeRestartProtocol = 0
	data, err := task.MarshalCanonical(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("runtime_restart_protocol")) {
		t.Fatal("legacy publication gained a capability field")
	}
	digest, err := legacy.Digest(f.intent, f.assumption)
	if err != nil || digest != task.ComputeSHA256(data) {
		t.Fatalf("legacy publication digest changed: %v", err)
	}
	current := legacy
	current.RuntimeRestartProtocol = runtimeRestartProtocolVersion
	if err = current.Validate(f.intent, f.assumption); err != nil {
		t.Fatal(err)
	}
	current.RuntimeRestartProtocol++
	if err = current.Validate(f.intent, f.assumption); !errors.Is(err, ErrIdentity) {
		t.Fatalf("unsupported restart protocol accepted: %v", err)
	}
}

func TestRuntimeRestartUnsupportedResidentRefusedBeforeContact(t *testing.T) {
	f := newOwnerContactFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	before := producerBudgetTree(t, f.journal.pin.Root())
	_, _, err := currentOwnerRequestAtStartup(ctx, f.reader, ownerRuntimeRestart, f.auth.request.Owner)
	if !errors.Is(err, ErrOwnerIncompatible) {
		t.Fatalf("legacy resident was contacted for restart: %v", err)
	}
	if !maps.Equal(before, producerBudgetTree(t, f.journal.pin.Root())) {
		t.Fatal("unsupported restart changed state")
	}
}

func TestRuntimeRestartWireReportsClosureWithoutFakeReady(t *testing.T) {
	from := ownerRequestFixture().Owner
	reply := ownerReplyFixture()
	reply.Action, reply.RestartFrom, reply.Startup = ownerRuntimeRestart, from, from
	reply.Target = originRecord{Root: "/accepted-root", RootID: reply.RootID}
	reply.RuntimeRestart = RuntimeRestartReport{From: from, Phase: RuntimeRestartDaemonClosed}
	if err := reply.validate(); err != nil {
		t.Fatal(err)
	}
	if reply.Disposition != (Disposition{}) {
		t.Fatal("quiescence fabricated a started/ready disposition")
	}
	changed := reply
	changed.RuntimeRestart.From.Epoch = strings.Repeat("f", 32)
	if err := changed.validate(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("reply changed original restart identity: %v", err)
	}
	request := ownerRequestFixture()
	request.Action, request.RestartFrom = ownerRuntimeRestart, from
	changed = reply
	changed.RestartFrom.Epoch = strings.Repeat("f", 32)
	if changed.matchesRequestedSubject(request) {
		t.Fatal("reply binding ignored restart identity")
	}
}

func TestRuntimeRestartRequestRequiresCurrentOrOutsideRecoveryPurpose(t *testing.T) {
	intent := startupIntentFixture(t)
	current, err := ReferenceStartup(intent)
	if err != nil {
		t.Fatal(err)
	}
	request := ownerRequest{Action: ownerRuntimeRestart, RestartFrom: current}
	if err = validateRuntimeRestartRequest(Operation{intent: intent}, request); err != nil {
		t.Fatalf("current startup was refused: %v", err)
	}
	request.RestartFrom = runtimeReferenceFixture().Startup
	if err = validateRuntimeRestartRequest(Operation{intent: intent}, request); !errors.Is(err, ErrOwnerRequest) {
		t.Fatalf("unbound predecessor was accepted: %v", err)
	}
	intent.Inputs.Initial = InitialOperation{Kind: InitialRuntimeRestart, RestartFrom: request.RestartFrom}
	intent.Inputs.RuntimeProducer = producerOutside
	refreshIntentDigest(t, &intent)
	if err = validateRuntimeRestartRequest(Operation{intent: intent}, request); err != nil {
		t.Fatalf("accepted outside recovery purpose was refused: %v", err)
	}
	request.RestartFrom.Epoch = strings.Repeat("f", 32)
	if err = validateRuntimeRestartRequest(Operation{intent: intent}, request); !errors.Is(err, ErrOwnerRequest) {
		t.Fatalf("recovery changed its original subject: %v", err)
	}
}
