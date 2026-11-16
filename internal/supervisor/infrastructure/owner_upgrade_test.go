package infrastructure

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"maps"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type upgradeTestHost struct {
	EntryHost
	execute func(context.Context, Operation, LaunchContext, UpgradeRequest, ResidentGuards, DaemonWaitOwner) (UpgradeOutcome, error)
}

func (h upgradeTestHost) UpgradeOwnRoot(ctx context.Context, op Operation, selected LaunchContext, request UpgradeRequest, guards ResidentGuards, waits DaemonWaitOwner) (UpgradeOutcome, error) {
	return h.execute(ctx, op, selected, request, guards, waits)
}

func upgradeRequestFixture(t *testing.T, f *ownerServerFixture, action UpgradeAction) ownerUpgradeRequest {
	t.Helper()
	legacy := f.request(t, ownerStatus)
	return ownerUpgradeRequest{
		Protocol: 5, RootID: legacy.RootID, Owner: legacy.Owner,
		PublicationSHA256: legacy.PublicationSHA256, Client: legacy.Client, Deadline: legacy.Deadline,
		Request: UpgradeRequest{Action: action, Context: f.entry.operation.defaultContextReference()},
	}
}

func readUpgradeReply(t *testing.T, f *ownerServerFixture, request ownerUpgradeRequest) ownerUpgradeReply {
	t.Helper()
	connection := f.dial(t)
	if err := writeOwnerFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	reply, err := readOwnerFrame[ownerUpgradeReply](bufio.NewReader(connection))
	if err != nil {
		t.Fatal(err)
	}
	if reply.subject() != request {
		t.Fatal("v5 changed an authenticated request subject")
	}
	return reply
}

func TestUpgradePreflightCompletesWithoutLegacyOutcomeOrHostEffects(t *testing.T) {
	var calls atomic.Int32
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{ensure: func(context.Context, Operation, LaunchContext, DaemonStartGuard, DaemonWaitOwner) (Disposition, error) {
		calls.Add(1)
		return Disposition{Kind: DispositionWarmReuse}, nil
	}}, ownerServerFixtureSelection{initial: InitialUpgradePreflight})
	before := producerBudgetTree(t, f.root)
	if disposition, err := f.server.prepareInitial(t.Context()); err != nil || disposition != (Disposition{}) {
		t.Fatalf("passive initial preflight: disposition=%+v err=%v", disposition, err)
	}
	if calls.Load() != 0 || f.server.initialOutcome != (PreparationOutcome{}) || f.server.terminalHandoff || f.server.runtimeFrom != (StartupReference{}) {
		t.Fatal("preflight entered legacy preparation or invented an outcome")
	}
	select {
	case <-f.server.initialDone:
	default:
		t.Fatal("preflight failed to wake shared initial waiters")
	}
	if !maps.Equal(before, producerBudgetTree(t, f.root)) {
		t.Fatal("initial preflight changed root bytes, modes or modification times")
	}
	if got := f.server.observeUpgradePreflight(); got.State != UpgradePreflightPassed {
		t.Fatalf("preflight did not pass: %+v", got)
	}
}

func TestUpgradePreflightFailureClosesWaitersWithoutLegacySuccess(t *testing.T) {
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{}, ownerServerFixtureSelection{initial: InitialUpgradePreflight})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.server.prepareInitial(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("failed preflight lost its cause: %v", err)
	}
	select {
	case <-f.server.initialDone:
	default:
		t.Fatal("failed preflight stranded initial waiters")
	}
	if got := f.server.observeUpgradePreflight(); got.State != UpgradePreflightFailed || f.server.initialOutcome != (PreparationOutcome{}) || !errors.Is(f.server.preflightError, context.Canceled) {
		t.Fatalf("failed preflight became legacy success: %+v", got)
	}
}

func TestUpgradePreflightLegacySelectorReturnsPromptNonSuccess(t *testing.T) {
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{}, ownerServerFixtureSelection{initial: InitialUpgradePreflight})
	f.start()
	for _, completed := range []bool{false, true} {
		if completed {
			if _, err := f.server.prepareInitial(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		request := f.request(t, ownerEnsureOwn)
		request.InitialResultFor = request.Owner
		connection := f.dial(t)
		if err := writeOwnerFrame(connection, request); err != nil {
			t.Fatal(err)
		}
		reply := readServerReply(t, connection)
		if reply.Code != ownerRefused && reply.Code != ownerIncompatible || reply.Disposition != (Disposition{}) {
			t.Fatalf("preflight legacy replay reported success/pending: %+v", reply)
		}
	}
}

func TestUpgradePreflightCallerLossPreservesIndependentLegacyPreparation(t *testing.T) {
	for _, initial := range []InitialOperationKind{InitialEnsureOwn, InitialUpgradePreflight} {
		t.Run(string(initial), func(t *testing.T) { checkIndependentLegacyPreparation(t, initial) })
	}
}

func checkIndependentLegacyPreparation(t *testing.T, initial InitialOperationKind) {
	t.Helper()
	var calls atomic.Int32
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{ensure: func(context.Context, Operation, LaunchContext, DaemonStartGuard, DaemonWaitOwner) (Disposition, error) {
		calls.Add(1)
		return Disposition{Kind: DispositionWarmReuse}, nil
	}}, ownerServerFixtureSelection{initial: initial})
	if _, err := f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	wantInitialCalls := int32(1)
	if initial == InitialUpgradePreflight {
		wantInitialCalls = 0
	}
	if calls.Load() != wantInitialCalls {
		t.Fatal("passive birth automatically prepared the supervisor")
	}
	// No v5 request follows birth: the initiating upgrade caller disappeared.
	// A later independently authenticated ordinary caller retains v4 authority.
	f.start()
	request := f.request(t, ownerEnsureOwn)
	connection := f.dial(t)
	if err := writeOwnerFrame(connection, request); err != nil {
		t.Fatal(err)
	}
	reply := readServerReply(t, connection)
	if reply.Code != ownerOK || reply.Disposition.Kind != DispositionWarmReuse || calls.Load() != wantInitialCalls+1 {
		t.Fatalf("birth history stranded independent ordinary work: code=%s host_calls=%d", reply.Code, calls.Load())
	}
	request.PublicationSHA256 = strings.Repeat("f", 64)
	unauthenticated := f.dial(t)
	if err := writeOwnerFrame(unauthenticated, request); err != nil {
		t.Fatal(err)
	}
	refusal, err := readOwnerFrame[ownerReply](bufio.NewReader(unauthenticated))
	if err != nil || refusal.Code != ownerRefused || calls.Load() != wantInitialCalls+1 {
		t.Fatalf("unauthenticated v4 request changed legacy refusal/effects: code=%s host_calls=%d err=%v", refusal.Code, calls.Load(), err)
	}
}

func TestUpgradeMutationsRequirePassedInitialGateAndTypedHost(t *testing.T) {
	for _, initial := range []InitialOperationKind{InitialUpgradePreflight, InitialEnsureOwn} {
		t.Run(string(initial), func(t *testing.T) { checkUpgradeInitialGate(t, initial) })
	}
}

func checkUpgradeInitialGate(t *testing.T, initial InitialOperationKind) {
	t.Helper()
	var calls atomic.Int32
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{}, ownerServerFixtureSelection{initial: initial})
	f.server.host = checkedUpgradeFixtureHost(t, f, &calls)
	f.start()
	request := upgradeRequestFixture(t, f, UpgradeEnsure)
	if reply := readUpgradeReply(t, f, request); reply.Outcome.Condition != UpgradePending || calls.Load() != 0 {
		t.Fatalf("pending initial gate admitted mutation: %+v calls=%d", reply, calls.Load())
	}
	if _, err := f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	reader := contextPassiveReader(t, f.entry.journal)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if passed, err := ProbeBoundUpgrade(ctx, reader, request.Owner); err != nil || passed.State != UpgradePreflightPassed || calls.Load() != 0 {
		t.Fatalf("status was effectful or failed: %+v err=%v", passed, err)
	}
	outcome, err := RequestBoundUpgrade(ctx, reader, request.Owner, UpgradeRequest{Action: UpgradeEnsure})
	if err != nil || outcome.Condition != UpgradeOK || calls.Load() != 1 {
		t.Fatalf("ordinary/preflight born owner failed later upgrade: %+v calls=%d err=%v", outcome, calls.Load(), err)
	}
}

func checkedUpgradeFixtureHost(t *testing.T, f *ownerServerFixture, calls *atomic.Int32) upgradeTestHost {
	t.Helper()
	return upgradeTestHost{EntryHost: f.server.host, execute: func(ctx context.Context, op Operation, selected LaunchContext, request UpgradeRequest, guards ResidentGuards, waits DaemonWaitOwner) (UpgradeOutcome, error) {
		calls.Add(1)
		if _, finite := ctx.Deadline(); !finite || op.Intent() != f.entry.operation.Intent() || selected.Root != f.root || guards != f.entry.grant || waits != f.server.waits || request.Action != UpgradeEnsure {
			t.Error("upgrade host lost the checked resident/request subjects")
		}
		return UpgradeOutcome{Condition: UpgradeOK, Disposition: Disposition{Kind: DispositionWarmReuse}}, nil
	}}
}

func TestUpgradeFailedOrdinaryPreparationNeverPassesGate(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{ensure: func(context.Context, Operation, LaunchContext, DaemonStartGuard, DaemonWaitOwner) (Disposition, error) {
		return Disposition{}, errors.New("initial preparation failed")
	}})
	var calls atomic.Int32
	f.server.host = upgradeTestHost{EntryHost: f.server.host, execute: func(context.Context, Operation, LaunchContext, UpgradeRequest, ResidentGuards, DaemonWaitOwner) (UpgradeOutcome, error) {
		calls.Add(1)
		return UpgradeOutcome{}, nil
	}}
	if _, err := f.server.prepareInitial(t.Context()); err == nil {
		t.Fatal("fixture initial preparation unexpectedly passed")
	}
	f.start()
	if reply := readUpgradeReply(t, f, upgradeRequestFixture(t, f, UpgradeEnsure)); reply.Outcome.Condition != UpgradeRefused || calls.Load() != 0 {
		t.Fatalf("failed initial preparation admitted upgrade: %+v calls=%d", reply, calls.Load())
	}
}

func TestUpgradeMissingHostReturnsExplicitUnsupported(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	if _, err := f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.start()
	if reply := readUpgradeReply(t, f, upgradeRequestFixture(t, f, UpgradeStatus)); reply.Outcome.Condition != UpgradeUnsupported || reply.Outcome.ReasonCode != "owner_upgrade_unsupported" {
		t.Fatalf("missing host silently accepted upgrade: %+v", reply)
	}
}

func TestUpgradeWireIsStrictAndCannotAliasLegacySubjects(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	request := upgradeRequestFixture(t, f, UpgradeStatus)
	var buffer bytes.Buffer
	if err := writeOwnerFrame(&buffer, request); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{`,"initial_result_for":{}`, `,"source":{}`, `,"initial_operation":{"kind":"upgrade-preflight"}`, `,"restart_from":{}`} {
		data := append(bytes.TrimSuffix(bytes.TrimSpace(buffer.Bytes()), []byte("}")), []byte(suffix+"}\n")...)
		if decoded, err := decodeOwnerFrame[ownerUpgradeRequest](data); err == nil || decoded != (ownerUpgradeRequest{}) {
			t.Fatalf("v5 accepted forbidden legacy subject %s: %+v %v", suffix, decoded, err)
		}
	}
	legacy := f.request(t, ownerEnsureOther)
	legacy.Target = originRecord{Root: "/other-root", RootID: strings.Repeat("f", 32)}
	legacy.Context = request.Request.Context
	legacy.Initial = InitialOperation{Kind: InitialUpgradePreflight}
	if err := legacy.validate(); err == nil {
		t.Fatal("v4 acquired the new preflight startup behavior")
	}
	request.Request.Action, request.Request.OperationID = UpgradeComplete, UpgradeOperationID("upgrade-v1:"+strings.Repeat("a", 32))
	if err := request.validate(); err != nil {
		t.Fatal(err)
	}
	reply := upgradeReply(request, UpgradeOutcome{Condition: UpgradePending, Preflight: UpgradePreflight{State: UpgradePreflightPassed}})
	if err := reply.validate(); err != nil {
		t.Fatal(err)
	}
	reply.Request.OperationID = "not-an-operation"
	if err := reply.validate(); err == nil {
		t.Fatal("v5 reply accepted a malformed operation echo")
	}
	if _, err := ParseUpgradeOperationID("upgrade-v1:" + strings.Repeat("A", 32)); err == nil {
		t.Fatal("operation ID parser accepted noncanonical bytes")
	}
	if err := (UpgradeOutcome{Condition: UpgradeOK, Preflight: UpgradePreflight{State: UpgradePreflightPassed}, Report: UpgradeReport{OperationID: UpgradeOperationID("upgrade-v1:" + strings.Repeat("a", 32)), Mode: UpgradeReactive, Phase: UpgradeDraining, Predecessor: task.SupervisorRef{}}}).Validate(); err == nil {
		t.Fatal("upgrade report invented a predecessor")
	}
}
