package infrastructure

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func restartDispositionReply(phase RuntimeRestartPhase) ownerReply {
	reply := ownerReplyFixture()
	from := reply.OwnerRef
	reply.Action, reply.RestartFrom = ownerRuntimeRestart, from
	reply.Target = originRecord{Root: "/accepted-root", RootID: reply.RootID}
	reply.Code = ownerPending
	reply.RuntimeRestart = RuntimeRestartReport{From: from, Phase: phase}
	if phase == RuntimeRestartSuccessorAccepted || phase == RuntimeRestartReady {
		reply.OwnerRef = StartupReference{Sequence: 2, Epoch: strings.Repeat("b", 32), IntentSHA256: strings.Repeat("c", 64)}
		reply.RuntimeRestart.Successor = reply.OwnerRef
	}
	if phase == RuntimeRestartDaemonClosed || phase == RuntimeRestartReady {
		reply.Code = ownerOK
	}
	reply.Startup = reply.OwnerRef
	return reply
}

func TestRuntimeRestartWireRefusesDispositionBeforeReady(t *testing.T) {
	for _, phase := range []RuntimeRestartPhase{RuntimeRestartRequested, RuntimeRestartShutdownPending, RuntimeRestartDaemonClosed, RuntimeRestartSuccessorAccepted} {
		t.Run(string(phase), func(t *testing.T) { assertRestartWireHasNoReadyDisposition(t, phase) })
	}
	for _, disposition := range []Disposition{{Kind: DispositionWarmReuse}, {Kind: DispositionStarted, Context: LaunchContextReference{SHA256: ownerWireDigest}}} {
		reply := restartDispositionReply(RuntimeRestartReady)
		reply.Disposition = disposition
		var output bytes.Buffer
		if err := writeOwnerFrame(&output, reply); err != nil {
			t.Fatalf("ready restart disposition was refused: %v", err)
		}
		decoded, err := readOwnerFrame[ownerReply](bufio.NewReader(&output))
		if err != nil || decoded != reply {
			t.Fatalf("ready restart wire changed: %+v %v", decoded, err)
		}
	}
}

func assertRestartWireHasNoReadyDisposition(t *testing.T, phase RuntimeRestartPhase) {
	t.Helper()
	base := restartDispositionReply(phase)
	if err := base.validate(); err != nil {
		t.Fatalf("invalid phase fixture before adding disposition: %v", err)
	}
	for _, disposition := range []Disposition{{Kind: DispositionWarmReuse}, {Kind: DispositionStarted, Context: LaunchContextReference{SHA256: ownerWireDigest}}} {
		reply := base
		reply.Disposition = disposition
		assertRestartWireRejectsReadyDisposition(t, reply)
		if err := reply.preparationOutcome().Validate(); !errors.Is(err, ErrIdentity) {
			t.Errorf("non-ready verified observation accepted a ready disposition: phase=%s err=%v", phase, err)
		}
	}
}

func assertRestartWireRejectsReadyDisposition(t *testing.T, reply ownerReply) {
	t.Helper()
	var output bytes.Buffer
	if err := writeOwnerFrame(&output, reply); !errors.Is(err, ErrIdentity) || output.Len() != 0 {
		t.Errorf("restart sender emitted an unready disposition: report=%+v err=%v", reply.RuntimeRestart, err)
	}
	data, err := task.MarshalCanonical(reply)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := readOwnerFrame[ownerReply](bufio.NewReader(bytes.NewReader(append(data, '\n'))))
	if !errors.Is(err, ErrIdentity) || decoded != (ownerReply{}) {
		t.Errorf("restart receiver accepted an unready disposition: report=%+v err=%v", reply.RuntimeRestart, err)
	}
}

func TestRuntimeRestartWireRefusesReadyDispositionWithoutReport(t *testing.T) {
	base := restartDispositionReply(RuntimeRestartRequested)
	base.RuntimeRestart = RuntimeRestartReport{}
	if err := base.validate(); err != nil {
		t.Fatalf("invalid pending fixture without disposition: %v", err)
	}
	for _, disposition := range []Disposition{{Kind: DispositionWarmReuse}, {Kind: DispositionStarted, Context: LaunchContextReference{SHA256: ownerWireDigest}}} {
		reply := base
		reply.Disposition = disposition
		assertRestartWireRejectsReadyDisposition(t, reply)
	}
}

func TestRuntimeRestartHostRefusesReadyDispositionBeforeReady(t *testing.T) {
	for _, phase := range []RuntimeRestartPhase{RuntimeRestartDaemonClosed, RuntimeRestartRequested} {
		t.Run(string(phase), func(t *testing.T) {
			f := newOwnerServerFixture(t, &ownerServerTestHost{restart: func(_ context.Context, _ Operation, _ LaunchContext, from StartupReference, _ ResidentGuards, _ DaemonWaitOwner) (Disposition, RuntimeRestartReport, error) {
				var cause error
				if phase == RuntimeRestartRequested {
					cause = ErrOwnerBusy
				}
				return Disposition{Kind: DispositionWarmReuse}, RuntimeRestartReport{From: from, Phase: phase}, cause
			}})
			request := f.request(t, ownerRuntimeRestart)
			request.RestartFrom = request.Owner
			work := acceptedOwnerWork{request: request, operation: f.entry.operation, selected: f.birthContext(t), selectedReference: f.entry.operation.defaultContextReference()}
			outcome, err := f.server.executePreparation(t.Context(), work)
			if !errors.Is(err, ErrIdentity) || outcome != (PreparationOutcome{}) {
				t.Fatalf("host ready disposition escaped before ready: phase=%s outcome=%+v err=%v", phase, outcome, err)
			}
		})
	}
}

func TestRuntimeRestartHostRefusesReadyDispositionWithoutReport(t *testing.T) {
	f := newOwnerServerFixture(t, &ownerServerTestHost{restart: func(context.Context, Operation, LaunchContext, StartupReference, ResidentGuards, DaemonWaitOwner) (Disposition, RuntimeRestartReport, error) {
		return Disposition{Kind: DispositionWarmReuse}, RuntimeRestartReport{}, nil
	}})
	request := f.request(t, ownerRuntimeRestart)
	request.RestartFrom = request.Owner
	work := acceptedOwnerWork{request: request, operation: f.entry.operation, selected: f.birthContext(t), selectedReference: f.entry.operation.defaultContextReference()}
	outcome, err := f.server.executePreparation(t.Context(), work)
	if !errors.Is(err, ErrIdentity) || outcome != (PreparationOutcome{}) {
		t.Fatalf("host ready disposition escaped without restart report: outcome=%+v err=%v", outcome, err)
	}
}
