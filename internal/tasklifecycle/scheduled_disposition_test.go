package tasklifecycle

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

func TestScheduledFenceReadersPreserveAutomaticCauseWithoutTerminalFacts(t *testing.T) {
	store, td, _ := lifecyclefixture.NewAppTestTask(t, false)
	t.Cleanup(func() { lifecyclefixture.CloseAppTestTask(t, store, td) })
	prepareScheduledSubmissionFailure(t, td)
	// This is an ordinary post-creation submission failure, without any submit
	// intent or provider execution. Status first observes authoritative storage.
	before, err := Status(store, td, SupervisorOptions{}, "", provider.Catalog{})
	if err != nil {
		t.Fatal(err)
	}
	if !before.AdmissionObserved || before.Admission != task.AdmissionNotAdmitted {
		t.Fatalf("pre-fence status: %+v", before)
	}
	proven, digest, err := FenceScheduledTaskContext(t.Context(), td)
	if err != nil || !proven || task.ValidateSHA256(digest) != nil {
		t.Fatalf("fence: %v %q %v", proven, digest, err)
	}
	status, err := Status(store, td, SupervisorOptions{}, "", provider.Catalog{})
	if err != nil {
		t.Fatal(err)
	}
	if !status.StopsRead || len(status.Stops) != 1 || status.Stops[0].Request.Cause != task.StopCauseAutomaticAbandonment {
		t.Fatalf("status lost truthful automatic refusal: %+v", status)
	}
	if status.LivenessObserved || status.Continuation != nil || status.Inspection.Outcome != nil {
		t.Fatalf("status fabricated execution closure: %+v", status)
	}
	assertScheduledFenceCollectionHasNoTerminalFacts(t, store.Root, td)
}

func prepareScheduledSubmissionFailure(t *testing.T, td *taskdir.TaskDir) {
	t.Helper()
	_, meta, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	conflicting := meta.SupervisorConfig
	conflicting.ConfigDigest = task.ComputeSHA256([]byte("different supervisor configuration"))
	permit, err := td.PrepareSubmission(conflicting)
	if permit != nil {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Fatal(releaseErr)
		}
	}
	if !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("ordinary submission validation failure: %v", err)
	}
}

func assertScheduledFenceCollectionHasNoTerminalFacts(t *testing.T, root string, td *taskdir.TaskDir) {
	t.Helper()
	collected, err := Collect(root, td, CollectionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if collected.Completion != nil || collected.Continuation != nil || !collected.Waiting {
		t.Fatalf("collection fabricated completion: %+v", collected)
	}
	if _, err = guardian.ReadClosureProof(t.Context(), td); !errors.Is(err, guardian.ErrProofPending) {
		t.Fatalf("admission refusal fabricated native closure proof: %v", err)
	}
	for _, name := range []string{"provider.start", "provider.exit", "outcome.json", filepath.Join("lifetime", "terminated.json"), filepath.Join("lifetime", "worker-wait.json")} {
		if _, err = os.Lstat(filepath.Join(td.Dir, name)); !os.IsNotExist(err) {
			t.Fatalf("automatic refusal fabricated %s: %v", name, err)
		}
	}
}

func TestScheduledFenceGuardianReplayRefusesBeforeAnyNativeWork(t *testing.T) {
	store, td, req := lifecyclefixture.NewAppTestTask(t, false)
	t.Cleanup(func() { lifecyclefixture.CloseAppTestTask(t, store, td) })
	proven, _, err := FenceScheduledTaskContext(t.Context(), td)
	if err != nil || !proven {
		t.Fatalf("fence: %v %v", proven, err)
	}
	server, client := net.Pipe()
	t.Cleanup(func() {
		if closeErr := errors.Join(server.Close(), client.Close()); closeErr != nil {
			t.Error(closeErr)
		}
	})
	scopeCalls, workerCalls := 0, 0
	owner, err := guardian.New(guardian.Options{
		RootID: req.RootID, TaskID: req.TaskID, Deadline: time.Now().Add(time.Minute), Records: td,
		AcquireOwner:   func(ctx context.Context) (guardian.Owner, error) { return td.AcquireLifetimeOwner(ctx) },
		ReconcileOwned: func(context.Context) error { return errors.New("refused replay must not recover native state") },
		Worker:         exec.Command("unused-refused-worker"), Connection: server,
		ScopeFactory: func(lifetime.Plan) (guardian.Scope, error) {
			scopeCalls++
			return nil, errors.New("refused replay must not create scope")
		},
		StartWorker: func(*exec.Cmd) error {
			workerCalls++
			return errors.New("refused replay must not start worker")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = owner.Run(t.Context()); !errors.Is(err, task.ErrTerminalTask) {
		t.Fatalf("guardian replay: %v", err)
	}
	if scopeCalls != 0 || workerCalls != 0 {
		t.Fatalf("automatic refusal launched native work: scopes=%d workers=%d", scopeCalls, workerCalls)
	}
}
