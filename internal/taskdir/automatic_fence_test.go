package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestAutomaticFenceCreatedTaskReplaysPermanentRefusal(t *testing.T) {
	store := testStore(t)
	td := supervisorTask(t, store)
	proven, digest, err := td.FenceUnlaunchedTaskContext(t.Context())
	must(t, err)
	rows, err := td.ReadStopRecords()
	must(t, err)
	if !proven || len(rows) != 1 || rows[0].Request.Cause != task.StopCauseAutomaticAbandonment {
		t.Fatalf("created task disposition: proven=%v digest=%q rows=%+v", proven, digest, rows)
	}
	request := rows[0].Request
	if request.NumericTaskID != nil || request.Deadline != "" || rows[0].Reply != nil || rows[0].Observation != nil {
		t.Fatalf("automatic refusal fabricated execution facts: %+v", rows[0])
	}
	data, err := task.MarshalCanonical(request)
	must(t, err)
	if digest != task.ComputeSHA256(data) {
		t.Fatalf("reference=%q; want canonical request hash", digest)
	}
	peer, err := store.OpenTask(td.TaskID)
	must(t, err)
	t.Cleanup(func() { must(t, peer.Close()) })
	again, second, err := peer.FenceUnlaunchedTaskContext(t.Context())
	must(t, err)
	if !again || second != digest {
		t.Fatalf("reopened replay: proven=%v digest=%q", again, second)
	}
	if _, err = peer.RecordLifetimeIntentContext(t.Context(), LifetimeIntentFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopePlan: "late-plan", BootID: "late-boot", Deadline: time.Now().Add(time.Hour)}); !errors.Is(err, task.ErrTerminalTask) {
		t.Fatalf("late lifetime intent: %v", err)
	}
	assertNoFabricatedExecution(t, td)
}

func assertNoFabricatedExecution(t *testing.T, td *TaskDir) {
	t.Helper()
	for _, name := range []string{"submit.json", "supervisor.ref.json", "provider.start", "provider.started.json", "provider.exit", "outcome.json", "result.txt", "publish.reject", filepath.Join(lifetimeDirectory, lifetimeIntentName), filepath.Join(lifetimeDirectory, lifetimeWorkerName), filepath.Join(lifetimeDirectory, lifetimeTerminationName), filepath.Join(lifetimeDirectory, lifetimeWorkerWaitName)} {
		if _, err := os.Lstat(filepath.Join(td.Dir, name)); !os.IsNotExist(err) {
			t.Fatalf("never-started disposition fabricated %s: %v", name, err)
		}
	}
}

func TestAutomaticFencePreservesExistingUserStop(t *testing.T) {
	td := supervisorTask(t, testStore(t))
	id := strings.Repeat("d", 32)
	permit, err := td.PrepareStop(id, "user", time.Time{})
	must(t, err)
	must(t, permit.Release())
	before, err := os.ReadFile(filepath.Join(td.Dir, "stop", id+stopRequestSuffix))
	must(t, err)
	proven, digest, err := td.FenceUnlaunchedTaskContext(t.Context())
	must(t, err)
	if !proven || digest != task.ComputeSHA256(before) {
		t.Fatalf("existing user refusal: proven=%v digest=%q", proven, digest)
	}
	rows, err := td.ReadStopRecords()
	must(t, err)
	if len(rows) != 1 || rows[0].Request.Cause != "user" {
		t.Fatalf("automatic path replaced existing user authority: %+v", rows)
	}
	assertNoFabricatedExecution(t, td)
}

func TestAutomaticFenceBlocksSubmissionAndExecutionEvidenceBeforeWrite(t *testing.T) {
	for _, state := range []string{"submission", "receipt", "started", "intent", "terminal"} {
		t.Run(state, func(t *testing.T) {
			td := supervisorTask(t, testStore(t))
			prepareAutomaticFenceBlockedState(t, td, state)
			proven, digest, err := td.FenceUnlaunchedTaskContext(t.Context())
			must(t, err)
			if proven || digest != "" {
				t.Fatalf("%s granted proof: %v %q", state, proven, digest)
			}
			rows, err := td.ReadStopRecords()
			must(t, err)
			if len(rows) != 0 {
				t.Fatalf("%s was automatically stopped: %+v", state, rows)
			}
		})
	}
}

func prepareAutomaticFenceBlockedState(t *testing.T, td *TaskDir, state string) {
	t.Helper()
	switch state {
	case "submission", "receipt":
		supervisorSubmit(t, td)
		if state == "receipt" {
			supervisorReceipt(t, td)
		}
	case "started", "terminal":
		start := consumeStart(t, td)
		if state == "terminal" {
			must(t, td.RecordStarted(0))
			must(t, td.WriteRawFiles("answer", ""))
			_, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
			must(t, err)
		}
		must(t, start.Release())
		if state == "terminal" {
			_, cleanup, err := td.Collect(task.FixturePredicateRef())
			must(t, errors.Join(cleanup, err))
		}
	case "intent":
		prepareCompletionTestIntent(t, td)
	}
}

func TestAutomaticFencePartialEvidenceNeverGrantsProof(t *testing.T) {
	for _, name := range []string{"brief.md", "task.json", "meta.json", "submit.json", "supervisor.ref.json", "provider.started.json", "provider.exit", "provider.ref.json", "outcome.json", filepath.Join(lifetimeDirectory, lifetimeRegistrationName), filepath.Join("stop", strings.Repeat("d", 32)+stopReplySuffix)} {
		t.Run(name, func(t *testing.T) {
			td := supervisorTask(t, testStore(t))
			path := filepath.Join(td.Dir, name)
			if name == "brief.md" || name == "task.json" || name == "meta.json" {
				must(t, os.Remove(path))
			} else {
				must(t, os.MkdirAll(filepath.Dir(path), 0o700))
				must(t, os.WriteFile(path, []byte("invalid evidence"), 0o600))
			}
			proven, digest, err := td.FenceUnlaunchedTaskContext(t.Context())
			if proven || digest != "" {
				t.Fatalf("partial %s granted proof: %v %q %v", name, proven, digest, err)
			}
			switch name {
			case "brief.md", "task.json", "meta.json", "submit.json", "outcome.json", filepath.Join(lifetimeDirectory, lifetimeRegistrationName), filepath.Join("stop", strings.Repeat("d", 32)+stopReplySuffix):
				if err == nil {
					t.Fatalf("partial %s did not surface its evidence fault", name)
				}
			}
			id := task.ComputeSHA256([]byte(task.StopCauseAutomaticAbandonment + ":" + td.TaskID))[:32]
			testAbsent(t, filepath.Join(td.Dir, "stop", id+stopRequestSuffix))
		})
	}
}

func TestBudgetRequestAloneIsNotPermanentLaunchRefusal(t *testing.T) {
	td := supervisorTask(t, testStore(t))
	start := consumeStart(t, td)
	must(t, start.Release())
	permit, err := td.PrepareStop("budget", "budget", time.Now().Add(time.Minute))
	must(t, err)
	must(t, permit.Release())
	refused, err := td.TaskLaunchRefusedContext(t.Context())
	must(t, err)
	if refused {
		t.Fatal("budget stop alone became a permanent refusal")
	}
	proven, digest, err := td.FenceUnlaunchedTaskContext(t.Context())
	must(t, err)
	if proven || digest != "" {
		t.Fatal("started budget-stopped task proved never started")
	}
}

func TestAutomaticFenceCancelledContextDoesNotWrite(t *testing.T) {
	td := supervisorTask(t, testStore(t))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	proven, digest, err := td.FenceUnlaunchedTaskContext(ctx)
	if proven || digest != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled fence: %v %q %v", proven, digest, err)
	}
	rows, err := td.ReadStopRecords()
	must(t, err)
	if len(rows) != 0 {
		t.Fatal("cancelled caller wrote a refusal")
	}
}

func TestAutomaticFenceUnexplainedOutputRemainsUnproven(t *testing.T) {
	for _, name := range []string{filepath.Join("raw", "stdout"), filepath.Join("raw", "stderr"), "result.txt", "publish.reject"} {
		t.Run(name, func(t *testing.T) {
			td := supervisorTask(t, testStore(t))
			path := filepath.Join(td.Dir, name)
			must(t, os.MkdirAll(filepath.Dir(path), 0o700))
			must(t, os.WriteFile(path, []byte("unexplained prior execution"), 0o600))
			proven, digest, err := td.FenceUnlaunchedTaskContext(t.Context())
			if proven || digest != "" {
				t.Fatalf("unexplained %s granted proof: %v %q %v", name, proven, digest, err)
			}
			rows, err := td.ReadStopRecords()
			must(t, err)
			if len(rows) != 0 {
				t.Fatal("unexplained execution was automatically stopped")
			}
		})
	}
}
