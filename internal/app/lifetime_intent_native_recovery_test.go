package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const nativeIntentGuardianLimit = time.Minute

var (
	errNativeIntentPlacement = errors.New("native intent placement sentinel")
	errNativeIntentScopeRan  = errors.New("native intent scope creation must not run")
	errNativeIntentWorkerRan = errors.New("native intent worker start must not run")
)

// TestStatusRecoversNativeIntentOnlyGuardianFailure composes a production
// guardian that fails custody placement after persisting its native intent
// with the public status command. Only the Pueue transport is a fixture; the
// plan, boot identity, records, owner lease, and recovery are production code.
func TestStatusRecoversNativeIntentOnlyGuardianFailure(t *testing.T) {
	fixture := newRebootFixture(t, rebootSetup{stage: rebootLifetimeNone})
	td := fixture.td
	runErr, selected := runNativeIntentGuardian(t, fixture)
	if !errors.Is(runErr, errNativeIntentPlacement) || !selected {
		t.Fatalf("guardian error = %v selected=%t, want placement sentinel after selection", runErr, selected)
	}
	intent := requireNativeIntentOnly(t, fixture)
	if _, err := guardian.ReadClosureProof(t.Context(), td); !errors.Is(err, guardian.ErrProofPending) {
		t.Fatalf("intent-only closure proof before status = %v, want %v", err, guardian.ErrProofPending)
	}
	before := snapshotRebootTaskFiles(t, td.Dir)

	first := requireNativeIntentInterruptedStatus(t, fixture)
	requireRebootTaskFilesUnchanged(t, before)
	requireNativeIntentRecovered(t, td, intent)
	recovered := snapshotRebootTaskFiles(t, td.Dir)

	// A repeat observation is stable: same envelope, no record rewritten.
	second := requireNativeIntentInterruptedStatus(t, fixture)
	requireRebootTaskFilesUnchanged(t, recovered)
	if len(recovered) != len(snapshotRebootTaskFiles(t, td.Dir)) {
		t.Fatal("repeat status observation added task records")
	}
	firstJSON, firstErr := json.Marshal(first)
	secondJSON, secondErr := json.Marshal(second)
	if firstErr != nil || secondErr != nil || string(firstJSON) != string(secondJSON) {
		t.Fatalf("repeat status differs:\nfirst  %s\nsecond %s\nerrors %v %v", firstJSON, secondJSON, firstErr, secondErr)
	}
}

// runNativeIntentGuardian runs one production guardian whose placement
// selector fails after the intent is durable. Scope creation and worker start
// are tripwires; neither may be reached.
func runNativeIntentGuardian(t *testing.T, fixture *rebootFixture) (error, bool) {
	t.Helper()
	td := fixture.td
	connection, peer := net.Pipe()
	t.Cleanup(func() {
		if err := errors.Join(connection.Close(), peer.Close()); err != nil {
			t.Errorf("close guardian control pipe: %v", err)
		}
	})
	selected := false
	owner, err := guardian.New(guardian.Options{
		RootID:   fixture.store.RootID,
		TaskID:   fixture.req.TaskID,
		Deadline: time.Now().Add(time.Hour),
		Records:  td,
		AcquireOwner: func(ctx context.Context) (guardian.Owner, error) {
			return td.AcquireLifetimeOwner(ctx)
		},
		ReconcileOwned: func(ctx context.Context) error {
			return guardian.ReconcileTaskLifetimeOwned(ctx, td)
		},
		Worker:     exec.Command(fixture.runner),
		Connection: connection,
		ScopeFactory: func(lifetime.Plan) (guardian.Scope, error) {
			t.Error(errNativeIntentScopeRan)
			return nil, errNativeIntentScopeRan
		},
		StartWorker: func(*exec.Cmd) error {
			t.Error(errNativeIntentWorkerRan)
			return errNativeIntentWorkerRan
		},
		SelectPlacement: func(ctx context.Context) (*task.LifetimeCustodyPlacement, error) {
			selected = true
			if intent, readErr := td.ReadLifetimeIntentContext(ctx); readErr != nil || intent == nil {
				t.Errorf("placement selected before durable intent: %+v err=%v", intent, readErr)
			}
			return nil, errNativeIntentPlacement
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), nativeIntentGuardianLimit)
	defer cancel()
	return owner.Run(ctx), selected
}

// requireNativeIntentOnly proves the guardian left exactly its actual native
// intent: the real Prepare plan and boot identity, and nothing after it.
func requireNativeIntentOnly(t *testing.T, fixture *rebootFixture) *task.LifetimeIntentRecord {
	t.Helper()
	evidence, err := taskdir.LoadLifetimeEvidenceContext(t.Context(), fixture.td)
	if err != nil {
		t.Fatal(err)
	}
	intent := evidence.Intent
	if intent == nil || evidence.Registration != nil || evidence.Worker != nil || evidence.WorkerWait != nil || len(evidence.Launches) != 0 || evidence.Termination != nil {
		t.Fatalf("guardian lifetime evidence = %+v, want intent only", evidence)
	}
	plan, err := lifetime.Prepare(fixture.store.RootID, task.LifetimeOwnerTask, fixture.req.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	bootID, err := lifetime.BootID()
	if err != nil {
		t.Fatal(err)
	}
	if intent.ScopeKind != string(plan.Kind()) || intent.Assurance != string(plan.Assurance()) || intent.ScopePlan != plan.Identifier() || intent.BootID != bootID {
		t.Fatalf("intent = %+v, want actual plan %s/%s %q on boot %q", intent, plan.Kind(), plan.Assurance(), plan.Identifier(), bootID)
	}
	t.Logf("native lifetime backend: goos=%s kind=%s assurance=%s; %s", runtime.GOOS, intent.ScopeKind, intent.Assurance, nativeIntentRecoveryPath(plan.Kind()))
	requireRebootNoProviderPublication(t, fixture)
	return intent
}

// nativeIntentRecoveryPath names what recovery inspects for the selected plan.
func nativeIntentRecoveryPath(kind lifetime.Kind) string {
	if kind == lifetime.KindCgroupV2 {
		return "recovery reopens the exact planned cgroup location"
	}
	return "an unregistered process-group plan has no prelaunch resource, so recovery reports it absent"
}

func requireNativeIntentInterruptedStatus(t *testing.T, fixture *rebootFixture) Response {
	t.Helper()
	code, response := runRebootStatusJSON(t, fixture)
	if code != 0 || response.Status != "interrupted" || response.Liveness != task.LivenessEnded.String() || response.Publication != task.PublicationUnknown.String() {
		t.Fatalf("status = code %d %+v, want interrupted/ended/publication unknown", code, response)
	}
	if response.Outcome != nil || response.Payload != nil || response.Pending != nil {
		t.Fatalf("status promised a result: %+v", response)
	}
	continuation := response.Continuation
	if continuation == nil || continuation.Resumable || continuation.ContinueCommand != "" || continuation.PredecessorID != fixture.req.TaskID {
		t.Fatalf("status continuation = %+v, want non-resumable predecessor %s", continuation, fixture.req.TaskID)
	}
	failure := response.Failure
	if failure == nil || failure.Code != "task_interrupted" || failure.NextAction != "stop_and_report" {
		t.Fatalf("status failure = %+v, want task_interrupted/stop_and_report", failure)
	}
	requireRebootNoProviderPublication(t, fixture)
	return response
}

// requireNativeIntentRecovered proves recovery published only the canonical
// absent-worker witness beside the unchanged intent.
func requireNativeIntentRecovered(t *testing.T, td *taskdir.TaskDir, intent *task.LifetimeIntentRecord) {
	t.Helper()
	evidence, err := taskdir.LoadLifetimeEvidenceContext(t.Context(), td)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Intent == nil || *evidence.Intent != *intent || evidence.Registration != nil || evidence.Worker != nil || len(evidence.Launches) != 0 || evidence.Termination != nil {
		t.Fatalf("recovered lifetime evidence = %+v, want unchanged intent without fabricated records", evidence)
	}
	requireNativeIntentAbsenceWitness(t, td, evidence.WorkerWait)
}

// requireNativeIntentAbsenceWitness requires the unregistered-disposed worker
// absence and the canonical closure proof it completes.
func requireNativeIntentAbsenceWitness(t *testing.T, td *taskdir.TaskDir, wait *task.LifetimeWorkerWaitRecord) {
	t.Helper()
	if wait == nil || wait.Result != task.LifetimeWorkerWaitAbsent || wait.AbsenceProof != task.LifetimeAbsenceUnregisteredDisposed || wait.ExitCode != nil || wait.ChildPID != 0 {
		t.Fatalf("worker witness = %+v, want unregistered-disposed absence", wait)
	}
	proof, err := guardian.ReadClosureProof(t.Context(), td)
	if err != nil || !proof.Closed() || proof.Kind() != task.LifetimeClosureUnregisteredRecovered || proof.TerminationSHA256() != "" || proof.WorkerWaitSHA256() == "" {
		t.Fatalf("closure proof = %+v err=%v, want closed unregistered-recovered", proof, err)
	}
}
