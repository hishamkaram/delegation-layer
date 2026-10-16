package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestPrivateSuccessfulCleanupPreservesInspectionExitBeforeRemoval(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reason    inspection.ResultReason
		succeeded bool
		wantErr   error
	}{
		{name: "eligible result and successful worker", reason: inspection.ResultEligible, succeeded: true},
		{name: "eligible result but failed worker", reason: inspection.ResultEligible, wantErr: errInspectionWorkerUnavailable},
		{name: "failed local inspection and worker", reason: inspection.ResultUnavailable, wantErr: errInspectionWorkerUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertCleanupInspectionExitSaved(t, tc.reason, tc.succeeded, tc.wantErr)
		})
	}
}

func TestPrivateSuccessfulCleanupRejectsUnsafeInspectionObservation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
	}{
		{name: "saved failure conflicts with observed success"},
		{name: "running row cannot establish worker exit", running: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertUnsafeInspectionCleanupRejected(t, tc.running)
		})
	}
}

func TestPrivateMigrationRejectsDuplicateInspectionLabelsBeforeObservation(t *testing.T) {
	for _, tc := range []struct {
		name          string
		matchingFirst bool
	}{
		{name: "matching receipt first", matchingFirst: true},
		{name: "duplicate first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertDuplicateInspectionMigrationRejected(t, tc.matchingFirst)
		})
	}
}

func TestFailedManagedInspectionWithoutCompletionAuthorizesCleanupAndMigration(t *testing.T) {
	for _, migrate := range []bool{false, true} {
		name := "cleanup"
		if migrate {
			name = "migration"
		}
		t.Run(name, func(t *testing.T) {
			assertFailedManagedInspectionWithoutCompletionAuthorized(t, migrate)
		})
	}
}

func assertFailedManagedInspectionWithoutCompletionAuthorized(t *testing.T, migrate bool) {
	t.Helper()
	store, operation, job := newCleanupInspectionTestOperationWithOwnership(
		t, inspection.ResultUnavailable, false, false, task.RunnerOwnershipManaged,
	)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	operationDirectory := filepath.Join(store.Root, "inspections", operation.Request().TaskID)
	for _, record := range []string{"completion.json", "result.json"} {
		if err := os.Remove(filepath.Join(operationDirectory, record)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := operation.ReadCompletedContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture retained local completion: %v", err)
	}

	binding := operation.Request().Binding.Supervisor
	if err := preparePrivateTerminalEvidence(t.Context(), store.Root, binding, []pueue.Job{job}); err != nil {
		t.Fatal(err)
	}
	if migrate {
		if err := authorizePrivateMigrationQueue(t.Context(), store.Root, binding, []pueue.Job{job}); err != nil {
			t.Fatalf("proven failed inspection blocked migration: %v", err)
		}
	} else {
		plan, err := authorizePrivateSuccessfulCleanup(t.Context(), store.Root, binding, []pueue.Job{job})
		if err != nil || len(plan.RemoveIDs) != 1 || plan.RemoveIDs[0] != job.ID || plan.Settling {
			t.Fatalf("proven failed inspection cleanup plan=%+v err=%v", plan, err)
		}
	}
	result, succeeded, err := operation.ReadWorkerResultContext(t.Context())
	if err != nil || succeeded || result != nil {
		t.Fatalf("failed inspection result=%+v succeeded=%t err=%v", result, succeeded, err)
	}
}

func TestManagedInspectionCleanupUsesGuardianEvidenceOverQueueResult(t *testing.T) {
	for _, migrate := range []bool{false, true} {
		name := "cleanup"
		if migrate {
			name = "migration"
		}
		t.Run(name, func(t *testing.T) {
			store, operation, job := newCleanupInspectionTestOperationWithOwnership(
				t, inspection.ResultEligible, false, false, task.RunnerOwnershipManaged,
			)
			completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
			binding := operation.Request().Binding.Supervisor
			if migrate {
				if err := authorizePrivateMigrationQueue(t.Context(), store.Root, binding, []pueue.Job{job}); err != nil {
					t.Fatalf("guardian queue failure blocked child-proven migration: %v", err)
				}
			} else {
				plan, err := authorizePrivateSuccessfulCleanup(t.Context(), store.Root, binding, []pueue.Job{job})
				if err != nil || len(plan.RemoveIDs) != 1 || plan.RemoveIDs[0] != job.ID || plan.Settling {
					t.Fatalf("guardian queue failure cleanup plan=%+v err=%v", plan, err)
				}
			}
			result, succeeded, err := operation.ReadWorkerResultContext(t.Context())
			if err != nil || !succeeded || result == nil || result.Reason != inspection.ResultEligible {
				t.Fatalf("guardian result=%+v succeeded=%t err=%v", result, succeeded, err)
			}
		})
	}
}

func assertCleanupInspectionExitSaved(t *testing.T, reason inspection.ResultReason, wantSucceeded bool, wantErr error) {
	t.Helper()
	store, operation, job := newCleanupInspectionTestOperation(t, reason, wantSucceeded)
	if wantSucceeded {
		completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	}
	request := operation.Request()
	if _, _, proofErr := operation.ReadWorkerResultContext(t.Context()); !errors.Is(proofErr, os.ErrNotExist) {
		t.Fatalf("fixture already has independent worker observation: %v", proofErr)
	}
	if err := preparePrivateTerminalEvidence(t.Context(), store.Root, request.Binding.Supervisor, []pueue.Job{job}); err != nil {
		t.Fatal(err)
	}
	plan, cleanupErr := authorizePrivateSuccessfulCleanup(t.Context(), store.Root, request.Binding.Supervisor, []pueue.Job{job})
	if cleanupErr != nil || len(plan.RemoveIDs) != 1 || plan.RemoveIDs[0] != job.ID || plan.Settling {
		t.Fatalf("cleanup did not authorize its completed inspection row: plan=%+v want=[%d] err=%v", plan, job.ID, cleanupErr)
	}
	result, succeeded, resultErr := operation.ReadWorkerResultContext(t.Context())
	if resultErr != nil || result.Reason != reason || succeeded != wantSucceeded {
		t.Fatalf("cleanup selected row before saving its exact worker result: result=%+v succeeded=%t err=%v", result, succeeded, resultErr)
	}
	assertCleanupInspectionTestProof(t, operation, wantSucceeded)

	// Once cleanup authorizes removal, admission must no longer need the queue
	// row. A nil supervisor makes any queue fallback invalid.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	facts, awaitErr := awaitInspection(ctx, operation, nil)
	if !errors.Is(awaitErr, wantErr) {
		t.Fatalf("admission did not finish from the saved worker result: facts=%s err=%v wantErr=%v", facts, awaitErr, wantErr)
	}
	wantFacts := ""
	if wantSucceeded {
		wantFacts = "{\"eligible\":true}\n"
	}
	if string(facts) != wantFacts {
		t.Fatalf("saved worker result changed admission facts: got=%s want=%s", facts, wantFacts)
	}
}

func assertUnsafeInspectionCleanupRejected(t *testing.T, running bool) {
	t.Helper()
	store, operation, job := newCleanupInspectionTestOperation(t, inspection.ResultEligible, true)
	if running {
		job.State = pueue.StateRunning
		job.Succeeded = false
	} else if recordErr := operation.RecordWorkerExitContext(t.Context(), job.ID, false); recordErr != nil {
		t.Fatalf("seed independently observed worker failure: %v", recordErr)
	}
	plan, cleanupErr := authorizePrivateSuccessfulCleanup(t.Context(), store.Root, operation.Request().Binding.Supervisor, []pueue.Job{job})
	if cleanupErr != nil || len(plan.RemoveIDs) != 0 || plan.Settling {
		t.Fatalf("unsafe inspection row was authorized for cleanup: plan=%+v err=%v", plan, cleanupErr)
	}
	result, succeeded, resultErr := operation.ReadWorkerResultContext(t.Context())
	if running {
		if !errors.Is(resultErr, os.ErrNotExist) {
			t.Fatalf("running row published worker-exit authority: result=%+v succeeded=%t err=%v", result, succeeded, resultErr)
		}
		return
	}
	if resultErr != nil || result.Reason != inspection.ResultEligible || succeeded {
		t.Fatalf("conflicting queue success replaced saved worker failure: result=%+v succeeded=%t err=%v", result, succeeded, resultErr)
	}
	assertCleanupInspectionTestProof(t, operation, false)
}

func assertDuplicateInspectionMigrationRejected(t *testing.T, matchingFirst bool) {
	t.Helper()
	store, operation, job := newCleanupInspectionTestOperation(t, inspection.ResultEligible, true)
	binding := operation.Request().Binding.Supervisor
	group, groupErr := inspection.OpenGroupContext(t.Context(), store, binding)
	if groupErr != nil {
		t.Fatal(groupErr)
	}
	if closeErr := group.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	duplicate := job
	duplicate.ID = 8
	jobs := []pueue.Job{job, duplicate}
	if !matchingFirst {
		jobs[0], jobs[1] = jobs[1], jobs[0]
	}
	migrationErr := authorizePrivateMigrationQueue(t.Context(), store.Root, binding, jobs)
	if !errors.Is(migrationErr, pueue.ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("migration accepted duplicate inspection labels: err=%v", migrationErr)
	}
	result, succeeded, resultErr := operation.ReadWorkerResultContext(t.Context())
	if !errors.Is(resultErr, os.ErrNotExist) {
		t.Fatalf("ambiguous migration published worker observation before rejecting the queue: result=%+v succeeded=%t err=%v", result, succeeded, resultErr)
	}
}

func newCleanupInspectionTestOperation(t *testing.T, reason inspection.ResultReason, succeeded bool) (*taskdir.Store, *inspection.Operation, pueue.Job) {
	t.Helper()
	return newCleanupInspectionTestOperationWithOwnership(t, reason, succeeded, false, "")
}

func newCleanupInspectionTestOperationInQueue(t *testing.T, reason inspection.ResultReason, succeeded, concurrent bool) (*taskdir.Store, *inspection.Operation, pueue.Job) {
	t.Helper()
	return newCleanupInspectionTestOperationWithOwnership(t, reason, succeeded, concurrent, "")
}

func newCleanupInspectionTestOperationWithOwnership(t *testing.T, reason inspection.ResultReason, succeeded, concurrent bool, ownership string) (*taskdir.Store, *inspection.Operation, pueue.Job) {
	t.Helper()
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	digest := strings.Repeat("a", 64)
	supervisor := continuationSupervisorRef(store.Root, digest, "unix:"+filepath.Join(store.Root, ".supervisor", "run", "pueue.sock"))
	openedAt := time.Now().UTC()
	binding := inspection.Binding{
		DefinitionRevision: "inspection-v1",
		DefinitionSHA256:   digest,
		HelperExecutable:   "/usr/bin/true",
		HelperSHA256:       digest,
		WorkerExecutable:   "/usr/bin/true",
		WorkerSHA256:       digest,
		RunnerOwnership:    ownership,
		Supervisor:         supervisor,
	}
	if ownership == task.RunnerOwnershipManaged {
		binding.WorkerExitProof = inspection.WorkerExitProofGuardianWaitV1
	}
	openOperation := inspection.OpenOperation
	if concurrent {
		binding.DefinitionRevision = commonprovider.RuntimeInspectionRevision
		binding.CapabilitySHA256 = digest
		openOperation = inspection.OpenConcurrentOperation
	}
	operation, err := openOperation(store, request, binding, openedAt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	submission, err := operation.ClaimSubmission(openedAt.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err = submission.ConsumeInspectionSubmission(); err != nil {
		t.Fatal(err)
	}
	const workerID = int64(7)
	if err = operation.RecordReceiptContext(t.Context(), workerID); err != nil {
		t.Fatal(err)
	}
	start, err := operation.ClaimStart(openedAt.Add(2 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	var facts []byte
	if reason == inspection.ResultEligible {
		facts = []byte(`{"eligible":true}`)
	}
	if err = operation.Complete(reason, facts, openedAt.Add(3*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	label := (pueue.InspectionIdentity{RootID: store.RootID, TaskID: request.TaskID}).Label()
	return store, operation, pueue.Job{
		ID: workerID, Label: &label, Group: operation.Request().Group,
		State: pueue.StateEnded, Succeeded: succeeded,
	}
}

func TestPrivateDefaultQueueInspectionCleanupAndMigrationNeedNoLegacyGroup(t *testing.T) {
	for _, migrate := range []bool{false, true} {
		name := "cleanup"
		if migrate {
			name = "migration"
		}
		t.Run(name, func(t *testing.T) {
			assertDefaultQueueCleanupAndMigration(t, migrate)
		})
	}
}

func TestTerminalInspectionDecisionDoesNotOwnQueueCleanup(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reason    inspection.ResultReason
		succeeded bool
		wantErr   error
	}{
		{name: "eligible success", reason: inspection.ResultEligible, succeeded: true},
		{name: "unavailable failure", reason: inspection.ResultUnavailable, wantErr: errInspectionWorkerUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertTerminalInspectionDecisionOnly(t, tc.reason, tc.succeeded, tc.wantErr)
		})
	}
}

func TestGuardianTerminalInspectionDecisionDoesNotOwnQueueCleanup(t *testing.T) {
	store := newBareInspectionStore(t)
	supervisor, removals, _ := newTerminalInspectionCleanupSupervisor(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	if err := operation.RecordWorkerSuccessContext(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	state := inspectionAwaitState{identity: operation.Request().SupervisorIdentity()}

	facts, ended, acceptErr := state.observe(t.Context(), operation, supervisor)
	if !ended || acceptErr != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("guardian-terminal inspection acceptance = facts=%s ended=%t err=%v", facts, ended, acceptErr)
	}
	if closeErr := supervisor.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if _, err := os.Lstat(removals); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("guardian-terminal admission mutated its supervisor row: %v", err)
	}
}

func TestPrivateCleanupReportsGuardianTerminalRunningRowAsSettling(t *testing.T) {
	store := newBareInspectionStore(t)
	supervisor, _, _ := newTerminalInspectionCleanupSupervisor(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, inspection.ResultEligible, true)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	if err := operation.RecordWorkerSuccessContext(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	job.State = pueue.StateRunning
	job.Succeeded = false

	plan, err := authorizePrivateSuccessfulCleanup(t.Context(), store.Root, operation.Request().Binding.Supervisor, []pueue.Job{job})
	if err != nil || len(plan.RemoveIDs) != 0 || !plan.Settling {
		t.Fatalf("guardian-terminal running row cleanup plan=%+v err=%v", plan, err)
	}
}

func assertTerminalInspectionDecisionOnly(t *testing.T, reason inspection.ResultReason, succeeded bool, wantErr error) {
	t.Helper()
	store := newBareInspectionStore(t)
	supervisor, removals, statusQueries := newTerminalInspectionCleanupSupervisor(t, store)
	operation, job := newTerminalInspectionCleanupOperation(t, store, supervisor, reason, succeeded)
	if succeeded {
		completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	}
	identity := operation.Request().SupervisorIdentity()

	facts, ended, acceptErr := acceptInspectionObservation(t.Context(), operation, supervisor, &identity, job)
	if !ended || !errors.Is(acceptErr, wantErr) {
		t.Fatalf("terminal inspection acceptance = facts=%s ended=%t err=%v, want err=%v", facts, ended, acceptErr, wantErr)
	}
	if succeeded && string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("successful inspection facts = %s", facts)
	}
	if !succeeded && facts != nil {
		t.Fatalf("failed inspection returned facts: %s", facts)
	}
	if closeErr := supervisor.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if _, err := os.Lstat(removals); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal inspection admission mutated its supervisor row: %v", err)
	}
	if _, err := os.Lstat(statusQueries); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal inspection admission started a cleanup observation: %v", err)
	}
	result, gotSucceeded, resultErr := operation.ReadWorkerResultContext(t.Context())
	if resultErr != nil || gotSucceeded != succeeded || result.Reason != reason {
		t.Fatalf("cleanup did not preserve terminal inspection evidence: result=%+v succeeded=%t err=%v", result, gotSucceeded, resultErr)
	}
}

func newTerminalInspectionCleanupSupervisor(t *testing.T, store *taskdir.Store) (*pueue.Client, string, string) {
	t.Helper()
	base := filepath.Join(store.Root, ".supervisor")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "cleanup-pueue")
	statusPath := filepath.Join(base, "cleanup-status.json")
	removals := filepath.Join(base, "cleanup-removals")
	statusQueries := filepath.Join(base, "cleanup-status-queries")
	script := `#!/bin/sh
set -eu
command=''
last=''
for arg in "$@"; do
  last="$arg"
  case "$arg" in
    --version|status|remove|add) command="$arg" ;;
  esac
done
case "$command" in
  --version) printf '%s\n' 'pueue 4.0.4' ;;
  status) printf '%s\n' "${5-}" >> "$FAKE_STATUS_QUERIES"; cat "$FAKE_STATUS" ;;
  remove) printf '%s\n' "$last" >> "$FAKE_REMOVALS" ;;
  add) printf '%s\n' '9' ;;
  *) exit 64 ;;
esac
`
	if err := os.WriteFile(clientPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := pueue.PrivateConfigPath(store.Root)
	config, err := json.Marshal(map[string]any{
		"shared": map[string]any{
			"pueue_directory":    filepath.Join(base, "data"),
			"runtime_directory":  filepath.Join(base, "run"),
			"unix_socket_path":   filepath.Join(base, "socket"),
			"alias_file":         filepath.Join(base, "aliases"),
			"pid_path":           filepath.Join(base, "pid"),
			"shared_secret_path": filepath.Join(base, "secret"),
			"daemon_cert":        filepath.Join(base, "cert"),
			"daemon_key":         filepath.Join(base, "key"),
		},
		"daemon": map[string]any{
			"shell_command": []string{"/bin/sh", "-c", "{{ pueue_command_string }}"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = pueue.WithBootstrapLock(t.Context(), store.Root, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	options := pueue.Options{
		Environment:                     append(os.Environ(), "FAKE_STATUS="+statusPath, "FAKE_REMOVALS="+removals, "FAKE_STATUS_QUERIES="+statusQueries),
		ObservationTimeout:              5 * time.Second,
		SuccessfulCleanupAuthorizer:     authorizePrivateSuccessfulCleanup,
		PrivateTerminalEvidencePreparer: preparePrivateTerminalEvidence,
	}
	client, err := pueue.Bind(t.Context(), clientPath, configPath, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return client, removals, statusQueries
}

func newTerminalInspectionCleanupOperation(t *testing.T, store *taskdir.Store, supervisor *pueue.Client, reason inspection.ResultReason, succeeded bool) (*inspection.Operation, pueue.Job) {
	return newTerminalInspectionCleanupOperationForTask(t, store, supervisor, reason, succeeded, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 7)
}

func newTerminalInspectionCleanupOperationForTask(t *testing.T, store *taskdir.Store, supervisor *pueue.Client, reason inspection.ResultReason, succeeded bool, taskID string, jobID int64) (*inspection.Operation, pueue.Job) {
	t.Helper()
	request := newBareInspectionRequest(t, store)
	request.TaskID = taskID
	openedAt := time.Now().UTC()
	digest := strings.Repeat("a", 64)
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1", DefinitionSHA256: digest,
		HelperExecutable: "/usr/bin/true", HelperSHA256: digest,
		WorkerExecutable: "/usr/bin/true", WorkerSHA256: digest,
		Supervisor: supervisor.Binding(),
	}, openedAt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	submission, err := operation.ClaimSubmission(openedAt.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err = submission.ConsumeInspectionSubmission(); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceiptContext(t.Context(), jobID); err != nil {
		t.Fatal(err)
	}
	start, err := operation.ClaimStart(openedAt.Add(2 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	var facts []byte
	if reason == inspection.ResultEligible {
		facts = []byte(`{"eligible":true}`)
	}
	if err = operation.Complete(reason, facts, openedAt.Add(3*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	identity := operation.Request().SupervisorIdentity()
	label := identity.Label()
	job := pueue.Job{ID: jobID, Label: &label, Group: identity.Group(), State: pueue.StateEnded, Succeeded: succeeded}
	status, err := json.Marshal(map[string]any{
		"tasks": map[string]any{strconv.FormatInt(jobID, 10): map[string]any{
			"id": jobID, "created_at": "2026-10-10T00:00:00Z", "original_command": "inspection-worker", "command": "inspection-worker",
			"path": "/tmp", "envs": map[string]string{}, "group": job.Group, "dependencies": []int64{}, "priority": 0,
			"label": label, "status": cleanupTestTerminalState(succeeded),
		}},
		"groups": map[string]any{job.Group: map[string]any{"status": "Running", "parallel_tasks": 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(store.Root, ".supervisor", "cleanup-status.json"), status, 0o600); err != nil {
		t.Fatal(err)
	}
	return operation, job
}

func cleanupTestTerminalState(succeeded bool) map[string]any {
	result := "Killed"
	if succeeded {
		result = "Success"
	}
	return map[string]any{"Done": map[string]any{
		"enqueued_at": "2026-10-10T00:00:00Z", "start": "2026-10-10T00:00:00Z", "end": "2026-10-10T00:00:00Z", "result": result,
	}}
}

func readCleanupTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertDefaultQueueCleanupAndMigration(t *testing.T, migrate bool) {
	t.Helper()
	store, operation, job := newCleanupInspectionTestOperationInQueue(t, inspection.ResultEligible, true, true)
	completeInspectionPollTestLifetime(t, operation, task.LifetimeTerminationCompleted)
	if job.Group != "default" {
		t.Fatalf("fixture used group %q", job.Group)
	}
	if _, _, err := inspection.ReadGroupHead(t.Context(), store); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture unexpectedly has a legacy group journal: %v", err)
	}
	binding := operation.Request().Binding.Supervisor
	if err := preparePrivateTerminalEvidence(t.Context(), store.Root, binding, []pueue.Job{job}); err != nil {
		t.Fatal(err)
	}
	if migrate {
		if err := authorizePrivateMigrationQueue(t.Context(), store.Root, binding, []pueue.Job{job}); err != nil {
			t.Fatalf("default-only inspection blocked migration: %v", err)
		}
	} else {
		plan, err := authorizePrivateSuccessfulCleanup(t.Context(), store.Root, binding, []pueue.Job{job})
		if err != nil || len(plan.RemoveIDs) != 1 || plan.RemoveIDs[0] != job.ID || plan.Settling {
			t.Fatalf("default inspection was not selected: plan=%+v err=%v", plan, err)
		}
	}
	assertCleanupInspectionTestProof(t, operation, true)
	facts, err := awaitInspection(t.Context(), operation, nil)
	if err != nil || string(facts) != "{\"eligible\":true}\n" {
		t.Fatalf("admission lost removed default worker evidence: facts=%s err=%v", facts, err)
	}
}

func assertCleanupInspectionTestProof(t *testing.T, operation *inspection.Operation, succeeded bool) {
	t.Helper()
	proof, proofErr := operation.ReadProofContext(t.Context())
	if !succeeded {
		if !errors.Is(proofErr, task.ErrEvidenceFault) {
			t.Fatalf("failed worker was promoted to eligible admission proof: proof=%s err=%v", proof, proofErr)
		}
		return
	}
	if proofErr != nil || string(proof) != "{\"eligible\":true}\n" {
		t.Fatalf("successful worker lacks canonical admission proof: proof=%s err=%v", proof, proofErr)
	}
}
