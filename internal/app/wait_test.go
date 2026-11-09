package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	waitFirstID  = "11111111111111111111111111111111"
	waitSecondID = "22222222222222222222222222222222"
	waitThirdID  = "33333333333333333333333333333333"
)

func TestWaitArgumentsValidateEveryIDAndMode(t *testing.T) {
	for _, rest := range [][]string{
		nil,
		{waitFirstID + " " + waitSecondID},
		{waitFirstID, "malformed"},
		{waitFirstID, waitFirstID},
		{"--any", "--all", waitFirstID},
		{"--watch", "-1s", waitFirstID},
	} {
		if _, err := parseWaitArguments(Arguments{Command: "wait"}, rest); err == nil {
			t.Errorf("accepted invalid wait argv %q", rest)
		}
	}
	args, err := parseWaitArguments(Arguments{Command: "wait"}, []string{waitFirstID, "--json", "--watch", "0s", waitSecondID})
	if err != nil || args.WaitAll || args.Watch != 0 || !args.JSON || !reflect.DeepEqual(args.WaitTaskIDs, []string{waitFirstID, waitSecondID}) {
		t.Fatalf("argument boundaries changed: %+v, %v", args, err)
	}
	defaults, err := parseWaitArguments(Arguments{Command: "wait"}, []string{waitFirstID})
	if err != nil || defaults.Watch != 5*time.Minute {
		t.Fatalf("default watch=%v, err=%v", defaults.Watch, err)
	}
}

type waitEvidenceFake struct {
	read func(context.Context, string) (taskdir.TaskEvidence, error)
}

func (f waitEvidenceFake) ReadTaskEvidence(ctx context.Context, id string) (taskdir.TaskEvidence, error) {
	return f.read(ctx, id)
}

func (waitEvidenceFake) ReadNeverStartedTaskContext(context.Context, string) (bool, string, error) {
	return false, "", nil
}

func waitPendingFacts(id string) taskdir.TaskEvidence {
	return taskdir.TaskEvidence{TaskID: id, Exists: true, Inspection: &taskdir.TaskInspection{Publication: task.PublicationUnknown}}
}

func waitPublishedFacts(id string, rejected bool) taskdir.TaskEvidence {
	facts := waitPendingFacts(id)
	facts.Inspection.Publication = task.PublicationCommitted
	if rejected {
		facts.Inspection.Publication = task.PublicationRejected
	}
	facts.Inspection.Outcome = &task.OutcomeRecord{}
	return facts
}

func TestWaitAnyReportsAllReadyTasksInInputOrder(t *testing.T) {
	reader := waitEvidenceFake{read: func(_ context.Context, id string) (taskdir.TaskEvidence, error) {
		if id == waitFirstID {
			return waitPendingFacts(id), nil
		}
		return waitPublishedFacts(id, id == waitThirdID), nil
	}}
	result := watchWaitTasks(t.Context(), Arguments{WaitTaskIDs: []string{waitFirstID, waitThirdID, waitSecondID}}, reader, newResponse("wait"), time.Second)
	if result.code != 0 || result.response.Wait == nil || !result.response.Wait.Satisfied {
		t.Fatalf("any wait did not complete: %+v", result)
	}
	tasks := result.response.Wait.Tasks
	if len(tasks) != 3 || tasks[0].Ready || tasks[1].TaskID != waitThirdID || !tasks[1].Ready || (tasks[1].Failure == nil || tasks[1].Failure.Code != "provider_output_rejected") || tasks[2].TaskID != waitSecondID || !tasks[2].Ready {
		t.Fatalf("any wait dropped, reordered, or misclassified tasks: %+v", tasks)
	}
}

func TestWaitAllNeedsEveryTaskAndObservesChanges(t *testing.T) {
	reads := map[string]int{}
	reader := waitEvidenceFake{read: func(_ context.Context, id string) (taskdir.TaskEvidence, error) {
		reads[id]++
		if id == waitSecondID && reads[id] == 1 {
			return waitPendingFacts(id), nil
		}
		return waitPublishedFacts(id, false), nil
	}}
	result := watchWaitTasks(t.Context(), Arguments{WaitAll: true, Watch: time.Second, WaitTaskIDs: []string{waitFirstID, waitSecondID}}, reader, newResponse("wait"), time.Nanosecond)
	if result.code != 0 || !result.response.Wait.Satisfied || reads[waitSecondID] < 2 || len(result.response.Wait.Tasks) != 2 {
		t.Fatalf("all wait completed before every task: %+v, reads=%v", result, reads)
	}
}

func TestWaitMissingOrFaultDoesNotHideBehindReadyTask(t *testing.T) {
	fault := errors.New("injected evidence corruption")
	for _, test := range []struct {
		name string
		err  error
		code string
	}{{"missing", nil, "task_not_found"}, {"fault", fault, "task_observation_failed"}} {
		t.Run(test.name, func(t *testing.T) {
			reader := waitEvidenceFake{read: func(_ context.Context, id string) (taskdir.TaskEvidence, error) {
				if id == waitFirstID {
					return waitPublishedFacts(id, false), nil
				}
				return taskdir.TaskEvidence{TaskID: id}, test.err
			}}
			result := watchWaitTasks(t.Context(), Arguments{WaitTaskIDs: []string{waitFirstID, waitSecondID}}, reader, newResponse("wait"), time.Second)
			if result.code != 1 || result.response.Failure == nil || result.response.Failure.Code != test.code {
				t.Fatalf("ready task hid %s: %+v", test.name, result)
			}
		})
	}
}

func TestWaitUndeterminedExpiresWithoutTreatingSealAsClosure(t *testing.T) {
	reader := waitEvidenceFake{read: func(_ context.Context, id string) (taskdir.TaskEvidence, error) {
		facts := waitPendingFacts(id)
		facts.Inspection.Publication = task.PublicationPending
		facts.Seal = &task.ProviderExitRecord{}
		return facts, nil
	}}
	for _, watch := range []time.Duration{0, time.Millisecond} {
		result := watchWaitTasks(t.Context(), Arguments{Watch: watch, WaitTaskIDs: []string{waitFirstID}}, reader, newResponse("wait"), time.Second)
		if result.code != 3 || result.response.Wait.Satisfied || len(result.response.Wait.Tasks) != 1 || result.response.Wait.Tasks[0].Ready {
			t.Fatalf("undetermined turn became ready for watch %s: %+v", watch, result)
		}
	}
}

func TestWaitWatchDeadlineBoundsTheRead(t *testing.T) {
	reader := waitEvidenceFake{read: func(ctx context.Context, id string) (taskdir.TaskEvidence, error) {
		<-ctx.Done()
		return taskdir.TaskEvidence{TaskID: id}, ctx.Err()
	}}
	result := watchWaitTasks(t.Context(), Arguments{Watch: time.Millisecond, WaitTaskIDs: []string{waitFirstID}}, reader, newResponse("wait"), time.Second)
	if result.code != 3 || result.response.Failure != nil {
		t.Fatalf("deadline became an operational fault: %+v", result)
	}
}

func TestWaitCLIRejectsInvalidInputBeforeOpeningState(t *testing.T) {
	missingRoot := filepath.Join(t.TempDir(), "missing")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"wait", "--root", missingRoot, "--json", waitFirstID, "invalid"}, &stdout, &stderr, Dependencies{})
	if code != 2 {
		t.Fatalf("invalid input exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	if _, err := os.Stat(missingRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid wait created state: %v", err)
	}
}

func TestWaitReadsRealSealedTaskWithoutPublishingOrProbing(t *testing.T) {
	store, td, _ := newAppTestTask(t, true)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	probe := filepath.Join(store.Root, ".probe")
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	marker := []byte("observation cannot probe")
	writeAppTestFile(t, probe, marker)
	result := waitTasks(Arguments{Root: store.Root, WaitTaskIDs: []string{td.TaskID}}, Dependencies{}.storeDependencies())
	if result.code != 3 || result.response.Wait == nil || result.response.Wait.Tasks[0].Ready {
		t.Fatalf("sealed task without closure became finished: %+v", result)
	}
	if _, err := os.Lstat(filepath.Join(td.Dir, "outcome.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wait published a result: %v", err)
	}
	got, err := os.ReadFile(probe)
	if err != nil || string(got) != string(marker) {
		t.Fatalf("wait probed or changed state: %q, %v", got, err)
	}
	var human strings.Builder
	writeHumanWait(&human, result.response.Wait)
	if !strings.Contains(human.String(), td.TaskID) || !strings.Contains(human.String(), "publication=pending") {
		t.Fatalf("human wait dropped observation: %s", &human)
	}
}

func TestWaitReadsRealPublishedTaskWithUndeterminedLiveness(t *testing.T) {
	store, td, _ := newAppTestTask(t, true)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	_, cleanupErr, collectErr := td.Collect(task.FixturePredicateRef())
	if err := errors.Join(cleanupErr, collectErr); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"wait", "--root", store.Root, "--watch", "0s", "--json", td.TaskID}, &stdout, &stderr, Dependencies{})
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("wait did not emit its JSON contract: %v stdout=%s stderr=%s", err, &stdout, &stderr)
	}
	if code != 0 || response.Command != "wait" || response.Wait == nil || !response.Wait.Satisfied || len(response.Wait.Tasks) != 1 {
		t.Fatalf("published task not ready through CLI: code=%d response=%+v stderr=%s", code, response, &stderr)
	}
	item := response.Wait.Tasks[0]
	if item.Publication != "committed" || item.Liveness != "undetermined" || !item.Ready {
		t.Fatalf("wait keyed completion on liveness: %+v", item)
	}
}

func TestWaitReadsCanonicalTimeoutWithoutPublishing(t *testing.T) {
	for _, test := range []struct {
		cause  string
		status string
		code   string
	}{{task.LifetimeTerminationBudget, "timed_out", "task_timed_out"}, {task.StopCauseOutputSilence, "idle_timed_out", "task_idle_timed_out"}} {
		t.Run(test.status, func(t *testing.T) {
			store, td, _ := newAppTestTask(t, false)
			t.Cleanup(func() { closeAppTestTask(t, store, td) })
			closeWaitTestLifetime(t, td, test.cause)
			result := waitTasks(Arguments{Root: store.Root, WaitTaskIDs: []string{td.TaskID}}, Dependencies{}.storeDependencies())
			if result.code != 0 || !result.response.Wait.Satisfied {
				t.Fatalf("closed timeout not ready: %+v", result)
			}
			item := result.response.Wait.Tasks[0]
			if item.Status != test.status || item.Failure == nil || item.Failure.Code != test.code || item.Readiness != "closed_without_outcome" || item.Publication != "unknown" {
				t.Fatalf("timeout evidence changed: %+v", item)
			}
			if _, err := os.Stat(filepath.Join(td.Dir, "outcome.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("wait fabricated timeout publication: %v", err)
			}
		})
	}
}

func closeWaitTestLifetime(t *testing.T, td *taskdir.TaskDir, cause string) {
	t.Helper()
	owner, err := td.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	deadline := time.Now().UTC().Add(time.Hour)
	_, err = td.RecordLifetimeIntentContext(t.Context(), taskdir.LifetimeIntentFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopePlan: "wait-plan", BootID: "wait-boot", Deadline: deadline})
	if err != nil {
		t.Fatal(err)
	}
	_, err = td.RecordLifetimeRegistrationContext(t.Context(), taskdir.LifetimeRegistrationFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopeID: "wait-scope", BootID: "wait-boot", Deadline: deadline})
	if err != nil {
		t.Fatal(err)
	}
	_, err = td.RecordLifetimeWorkerContext(t.Context(), taskdir.LifetimeWorkerFacts{PID: 1234, ProcessStart: "wait-start", BootID: "wait-boot"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = td.RecordLifetimeTerminationContext(t.Context(), cause)
	if err != nil {
		t.Fatal(err)
	}
	_, err = td.RecordLifetimeWorkerAbsenceContext(t.Context(), task.LifetimeAbsenceExactWorker, "wait-boot")
	if err != nil {
		t.Fatal(err)
	}
	if err = owner.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestWaitReadsSettledNeverStartedTask(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	proven, _, err := td.FenceUnlaunchedTaskContext(t.Context())
	if err != nil || !proven {
		t.Fatalf("fixture fence=%t error=%v", proven, err)
	}
	result := waitTasks(Arguments{Root: store.Root, WaitTaskIDs: []string{td.TaskID}}, Dependencies{}.storeDependencies())
	if result.code != 0 || !result.response.Wait.Tasks[0].Ready || result.response.Wait.Tasks[0].Readiness != "never_started" {
		t.Fatalf("never-started turn not ready: %+v", result)
	}
	for _, name := range []string{"provider.start", "provider.exit", "outcome.json"} {
		if _, err = os.Stat(filepath.Join(td.Dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("wait created %s: %v", name, err)
		}
	}
}

func TestWaitMissingRootDoesNotInitializeIt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	result := waitTasks(Arguments{Root: root, WaitTaskIDs: []string{waitFirstID}}, Dependencies{}.storeDependencies())
	if result.code != 1 || result.response.Failure == nil || result.response.Failure.Code != "task_not_found" {
		t.Fatalf("missing root classification: %+v", result)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wait initialized missing root: %v", err)
	}
}

func TestWaitDoesNotMistakeAnEvidenceDeadlineForWatchExpiry(t *testing.T) {
	reader := waitEvidenceFake{read: func(context.Context, string) (taskdir.TaskEvidence, error) {
		return taskdir.TaskEvidence{}, context.DeadlineExceeded
	}}
	result := watchWaitTasks(t.Context(), Arguments{Watch: time.Hour, WaitTaskIDs: []string{waitFirstID}}, reader, newResponse("wait"), time.Second)
	if result.code != 1 || result.response.Failure == nil || result.response.Failure.Code != "task_observation_failed" || !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatalf("evidence fault became watch expiry: %+v", result)
	}
}

func TestWaitRedactsEvidenceDiagnosticsAndPreservesCause(t *testing.T) {
	secret := errors.New("provider error containing private output")
	reader := waitEvidenceFake{read: func(context.Context, string) (taskdir.TaskEvidence, error) {
		return taskdir.TaskEvidence{}, secret
	}}
	result := watchWaitTasks(t.Context(), Arguments{WaitTaskIDs: []string{waitFirstID}}, reader, newResponse("wait"), time.Second)
	if result.code != 1 || !errors.Is(result.err, secret) || strings.Contains(result.response.Error, secret.Error()) || result.response.TaskID != waitFirstID {
		t.Fatalf("evidence failure lost safe typed context: %+v", result)
	}
}

func TestWaitDeadlineDoesNotHideConcreteEvidenceFault(t *testing.T) {
	reader := waitEvidenceFake{read: func(ctx context.Context, id string) (taskdir.TaskEvidence, error) {
		<-ctx.Done()
		return taskdir.TaskEvidence{TaskID: id}, errors.Join(ctx.Err(), task.ErrEvidenceFault)
	}}
	result := watchWaitTasks(t.Context(), Arguments{Watch: time.Millisecond, WaitTaskIDs: []string{waitFirstID}}, reader, newResponse("wait"), time.Second)
	if result.code != 1 || result.response.Failure == nil || result.response.Failure.Code != "task_observation_failed" || !errors.Is(result.err, task.ErrEvidenceFault) {
		t.Fatalf("deadline hid concrete evidence fault: %+v", result)
	}
}
