package infrastructure

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadyCurrentRequiresExactCompletedOperation(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	request := upgradeRequestFixture(t, f, UpgradeReadyCurrent)
	if err := request.validate(); !errors.Is(err, ErrIdentity) {
		t.Fatalf("ReadyCurrent accepted absent A: %v", err)
	}
	request.Request.OperationID = UpgradeOperationID("upgrade-v1:" + strings.Repeat("a", 32))
	if err := request.validate(); err != nil {
		t.Fatal(err)
	}
	ready := UpgradeOutcome{Condition: UpgradeOK, Preflight: UpgradePreflight{State: UpgradePreflightPassed}, Disposition: Disposition{Kind: DispositionWarmReuse}}
	if err := validateUpgradeOutcomeSubject(request.Request, ready); !errors.Is(err, ErrIdentity) {
		t.Fatalf("ReadyCurrent succeeded without completed A: %v", err)
	}
	// An owner can refuse before opening canonical A; that is never success.
	refused := UpgradeOutcome{Condition: UpgradeRefused, Preflight: ready.Preflight, ReasonCode: "not_current"}
	if err := validateUpgradeOutcomeSubject(request.Request, refused); err != nil {
		t.Fatal(err)
	}
}

func TestReadyCurrentReachesOnlyAuthenticatedUpgradeHost(t *testing.T) {
	var calls atomic.Int32
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{}, ownerServerFixtureSelection{initial: InitialUpgradePreflight})
	a := UpgradeOperationID("upgrade-v1:" + strings.Repeat("a", 32))
	f.server.host = upgradeTestHost{EntryHost: f.server.host, execute: func(_ context.Context, _ Operation, _ LaunchContext, request UpgradeRequest, _ ResidentGuards, _ DaemonWaitOwner) (UpgradeOutcome, error) {
		calls.Add(1)
		if request.Action != UpgradeReadyCurrent || request.OperationID != a {
			t.Error("ReadyCurrent lost exact action/A")
		}
		return UpgradeOutcome{Condition: UpgradeRefused, ReasonCode: "not_current"}, nil
	}}
	if _, err := f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.start()
	request := upgradeRequestFixture(t, f, UpgradeReadyCurrent)
	request.Request.OperationID = a
	reply := readUpgradeReply(t, f, request)
	if reply.Outcome.Condition != UpgradeRefused || calls.Load() != 1 {
		t.Fatalf("ReadyCurrent did not reach checked owner: %+v calls=%d", reply, calls.Load())
	}
	request.Request.Action = "unknown-readiness"
	if err := request.validate(); !errors.Is(err, ErrIdentity) || calls.Load() != 1 {
		t.Fatalf("unknown action acquired authority: %v", err)
	}
}

func TestReadyCurrentUnsupportedOwnerHasNoOrdinaryFallback(t *testing.T) {
	var ordinary atomic.Int32
	f := newOwnerServerFixtureWithSelection(t, &ownerServerTestHost{ensure: func(context.Context, Operation, LaunchContext, DaemonStartGuard, DaemonWaitOwner) (Disposition, error) {
		ordinary.Add(1)
		return Disposition{Kind: DispositionWarmReuse}, nil
	}}, ownerServerFixtureSelection{initial: InitialUpgradePreflight})
	if _, err := f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.start()
	reader := contextPassiveReader(t, f.entry.journal)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request := upgradeRequestFixture(t, f, UpgradeReadyCurrent)
	request.Request.OperationID = UpgradeOperationID("upgrade-v1:" + strings.Repeat("a", 32))
	outcome, err := RequestBoundUpgrade(ctx, reader, request.Owner, request.Request)
	if !errors.Is(err, ErrOwnerIncompatible) || outcome.Condition != UpgradeUnsupported || ordinary.Load() != 0 {
		t.Fatalf("unsupported ReadyCurrent fell back: outcome=%+v ordinary=%d err=%v", outcome, ordinary.Load(), err)
	}
}
