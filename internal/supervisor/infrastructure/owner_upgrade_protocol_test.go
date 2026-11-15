package infrastructure

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestUpgradeWireMatchesIndependentV5Fixture(t *testing.T) {
	legacy := ownerRequestFixture()
	request := ownerUpgradeRequest{
		Protocol: 5, RootID: legacy.RootID, Owner: legacy.Owner, PublicationSHA256: legacy.PublicationSHA256,
		Client: legacy.Client, Deadline: legacy.Deadline, Request: UpgradeRequest{Action: UpgradeStatus, Context: LaunchContextReference{SHA256: ownerWireDigest}},
	}
	wire := `{"protocol":5,"root_id":"` + ownerWireRoot + `","owner":{"sequence":1,"epoch":"` + ownerWireRoot + `","intent_sha256":"` + ownerWireDigest + `"},"publication_sha256":"` + ownerWireDigest + `","client":{"pid":7,"start_token":"fixture:7"},"deadline":"2026-11-03T00:00:30Z","request":{"action":"upgrade-status","context":{"sha256":"` + ownerWireDigest + `"}}}` + "\n"
	var output bytes.Buffer
	if err := writeOwnerFrame(&output, request); err != nil {
		t.Fatal(err)
	}
	if output.String() != wire {
		t.Fatalf("v5 frame changed independently specified shape: %s", output.String())
	}
	decoded, err := decodeOwnerFrame[ownerUpgradeRequest]([]byte(wire))
	if err != nil || decoded != request {
		t.Fatalf("v5 fixture refused: %+v %v", decoded, err)
	}
	if _, err = decodeOwnerFrame[ownerRequest]([]byte(wire)); err == nil {
		t.Fatal("v5 fixture aliased a legacy preparation request")
	}
	reply := upgradeReply(request, UpgradeOutcome{Condition: UpgradeOK, Preflight: UpgradePreflight{State: UpgradePreflightPassed}})
	output.Reset()
	if err = writeOwnerFrame(&output, reply); err != nil {
		t.Fatal(err)
	}
	replyWire := strings.TrimSuffix(wire, "}\n") + `,"outcome":{"condition":"ok","preflight":{"state":"passed"}}}` + "\n"
	if output.String() != replyWire {
		t.Fatalf("v5 status reply changed independent golden: %s", output.String())
	}
}

func TestUpgradeAuthenticationAndContextFailuresHaveNoHostEffects(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	if _, err := f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	f.server.host = upgradeTestHost{EntryHost: f.server.host, execute: func(context.Context, Operation, LaunchContext, UpgradeRequest, ResidentGuards, DaemonWaitOwner) (UpgradeOutcome, error) {
		calls.Add(1)
		return UpgradeOutcome{Condition: UpgradeOK, Disposition: Disposition{Kind: DispositionWarmReuse}}, nil
	}}
	f.start()
	for _, mismatch := range []string{"root", "startup", "publication", "client", "context"} {
		request := upgradeRequestFixture(t, f, UpgradeEnsure)
		switch mismatch {
		case "root":
			request.RootID = strings.Repeat("f", 32)
		case "startup":
			request.Owner.IntentSHA256 = strings.Repeat("f", 64)
		case "publication":
			request.PublicationSHA256 = strings.Repeat("f", 64)
		case "client":
			request.Client.StartToken = "foreign-incarnation"
		case "context":
			request.Request.Context.SHA256 = strings.Repeat("f", 64)
		}
		connection := f.dial(t)
		if err := writeOwnerFrame(connection, request); err != nil {
			t.Fatal(err)
		}
		reply, err := readOwnerFrame[ownerUpgradeReply](bufio.NewReader(connection))
		if mismatch == "context" {
			if err != nil || reply.Outcome.Condition != UpgradeRefused {
				t.Fatalf("context mismatch was not a known refusal: %+v %v", reply, err)
			}
		} else if err == nil {
			t.Fatalf("unauthenticated %s received a bound reply: %+v", mismatch, reply)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("foreign request reached the upgrade host")
	}
}

func TestUpgradeLostAcknowledgmentDoesNotCancelOrReplayAcceptedWork(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	if _, err := f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	entered, release, completed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	f.server.host = upgradeTestHost{EntryHost: f.server.host, execute: func(accepted context.Context, _ Operation, _ LaunchContext, _ UpgradeRequest, _ ResidentGuards, _ DaemonWaitOwner) (UpgradeOutcome, error) {
		calls.Add(1)
		if actual, finite := accepted.Deadline(); !finite || !actual.Equal(deadline) {
			t.Error("upgrade replaced the original control deadline")
		}
		close(entered)
		<-release
		if accepted.Err() != nil {
			t.Error("socket observer cancellation canceled accepted upgrade work")
		}
		close(completed)
		return UpgradeOutcome{Condition: UpgradeOK, Disposition: Disposition{Kind: DispositionWarmReuse}}, nil
	}}
	f.start()
	reader := contextPassiveReader(t, f.entry.journal)
	reference, err := ReferenceStartup(f.entry.operation.Intent())
	if err != nil {
		t.Fatal(err)
	}
	returned := make(chan error, 1)
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		outcome, contactErr := RequestBoundUpgrade(ctx, reader, reference, UpgradeRequest{Action: UpgradeEnsure})
		if outcome != (UpgradeOutcome{}) {
			contactErr = errors.Join(contactErr, errors.New("lost acknowledgment fabricated a verified result"))
		}
		returned <- contactErr
	}()
	defer func() { cancel(); unblock(); <-clientDone }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("upgrade did not reach the accepted mutation edge")
	}
	cancel()
	requireCanceledUpgradeClient(t, returned)
	unblock()
	select {
	case <-completed:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted work was not completed")
	}
	if err = f.server.stopAcceptAndDrain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("uncertain upgrade was replayed: calls=%d", calls.Load())
	}
}

func requireCanceledUpgradeClient(t *testing.T, returned <-chan error) {
	t.Helper()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lost acknowledgement did not remain unknown/canceled: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled client was not joined")
	}
}

type upgradeHostFaultCase struct {
	name      string
	condition UpgradeCondition
	want      UpgradeCondition
	fault     bool
	retained  bool
}

func TestUpgradeHostUnhandledErrorsOverridePendingWithoutLosingProgress(t *testing.T) {
	for _, test := range []upgradeHostFaultCase{
		{name: "handled-pending", condition: UpgradePending, want: UpgradePending},
		{name: "pending-with-fs-fault", condition: UpgradePending, want: UpgradeFault, fault: true},
		{name: "busy-with-retained-cleanup", condition: UpgradeBusy, want: UpgradeFault, fault: true, retained: true},
	} {
		t.Run(test.name, func(t *testing.T) { checkUpgradeHostFault(t, test) })
	}
}

func checkUpgradeHostFault(t *testing.T, test upgradeHostFaultCase) {
	t.Helper()
	f := newOwnerServerFixture(t, &ownerServerTestHost{})
	if _, err := f.server.prepareInitial(t.Context()); err != nil {
		t.Fatal(err)
	}
	progress := UpgradeReport{
		OperationID: "upgrade-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Mode: UpgradeReactive, Phase: UpgradeDraining,
		Predecessor: task.SupervisorRef{ConfigPath: filepath.Join(f.root, "supervisor.yml"), ConfigDigest: ownerWireDigest, Endpoint: "fixture-private-endpoint", ObservedVersion: "fixture-v1"},
	}
	fault := errors.New("independent filesystem or cleanup fault")
	var hostError error
	if test.fault {
		hostError = fault
	}
	if test.retained {
		hostError = &residentEntryCleanupError{entry: f.entry, cause: fault}
		f.expectedCleanupFault = fault
	}
	f.server.host = upgradeTestHost{EntryHost: f.server.host, execute: func(context.Context, Operation, LaunchContext, UpgradeRequest, ResidentGuards, DaemonWaitOwner) (UpgradeOutcome, error) {
		return UpgradeOutcome{Condition: test.condition, Report: progress}, hostError
	}}
	f.start()
	reader := contextPassiveReader(t, f.entry.journal)
	reference, err := ReferenceStartup(f.entry.operation.Intent())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	outcome, err := RequestBoundUpgrade(ctx, reader, reference, UpgradeRequest{Action: UpgradeEnsure})
	if err != nil || outcome.Condition != test.want || outcome.Report != progress {
		t.Fatalf("unhandled Host error was hidden as pending or lost progress: condition=%s want=%s progress=%+v err=%v", outcome.Condition, test.want, outcome.Report, err)
	}
	if f.server.isClosing() != test.retained {
		t.Fatal("retained cleanup fault did not preserve admission closure")
	}
	if test.retained {
		var retained *residentEntryCleanupError
		if !errors.As(f.server.fault(), &retained) || retained.entry != f.entry || !errors.Is(retained, fault) {
			t.Fatal("Host cleanup fault lost the actual owner graph")
		}
	}
}
