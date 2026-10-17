package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestLegacyActiveWorkIncludesUnsafeJobStates(t *testing.T) {
	if got := legacyActiveWork(pueue.QueueSnapshot{Jobs: []pueue.Job{{Group: "default", State: pueue.StateRunning}}}); got != "present" {
		t.Fatalf("running ordinary work was not reported: %q", got)
	}
	if got := legacyActiveWork(pueue.QueueSnapshot{Jobs: []pueue.Job{{Group: "default", State: pueue.StateUnknown}}}); got != "present" {
		t.Fatalf("unknown legacy work was treated as safe: %q", got)
	}
	if got := legacyActiveWork(pueue.QueueSnapshot{Jobs: []pueue.Job{{Group: "default", State: pueue.StateEnded}}}); got != "none" {
		t.Fatalf("ended work was reported as active: %q", got)
	}
}

func TestAuthorizePrivateSuccessfulCleanupOwnsAllEvidenceFamilies(t *testing.T) {
	store := newBareInspectionStore(t)
	root := store.Root
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := authorizePrivateSuccessfulCleanup(context.Background(), root, task.SupervisorRef{}, nil); err != nil {
		t.Fatalf("empty state root was not authorizable: %v", err)
	}
	taskID := strings.Repeat("a", 32)
	if err := os.MkdirAll(filepath.Join(root, "tasks", taskID), 0o700); err != nil {
		t.Fatal(err)
	}
	if plan, err := authorizePrivateSuccessfulCleanup(context.Background(), root, task.SupervisorRef{}, nil); err != nil || len(plan.RemoveIDs) != 0 || plan.Settling {
		t.Fatalf("unrelated incomplete history affected empty queue cleanup: plan=%+v err=%v", plan, err)
	}
}

func TestAuthorizePrivateMigrationQueueOwnsTerminalEvidence(t *testing.T) {
	store := newBareInspectionStore(t)
	root := store.Root
	rootID := store.RootID
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	taskID := strings.Repeat("b", 32)
	if err := os.MkdirAll(filepath.Join(root, "tasks", taskID), 0o700); err != nil {
		t.Fatal(err)
	}
	label := "delegate:" + rootID + ":" + taskID
	if err := authorizePrivateMigrationQueue(context.Background(), root, task.SupervisorRef{}, []pueue.Job{{ID: 7, Label: &label, State: pueue.StateEnded}}); !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("ended task without terminal evidence was authorized: %v", err)
	}
	if err := authorizePrivateMigrationQueue(context.Background(), root, task.SupervisorRef{}, []pueue.Job{{ID: 7, Label: &label, State: pueue.StateRunning}}); !errors.Is(err, pueue.ErrPrivateSupervisorMigrationBusy) {
		t.Fatalf("active migration work was not blocked: %v", err)
	}
}

func TestLegacySupervisorStatusFailsClosedOnReadinessError(t *testing.T) {
	response := newResponse("supervisor")
	response.SupervisorControl = &SupervisorControlResponse{}

	result, done := legacySupervisorStateResult(
		response,
		inspection.GroupRequestRecord{},
		true,
		true,
		false,
		errors.New("readiness failed"),
		false,
	)
	if !done {
		t.Fatal("readiness failure was not handled")
	}
	if result.code == 0 {
		t.Fatal("unreachable migrated supervisor was reported as healthy")
	}
	if result.response.SupervisorControl.State != "unavailable" ||
		result.response.SupervisorControl.LegacyReachability != "incompatible" ||
		result.response.SupervisorControl.ActiveWork != "unknown" ||
		result.response.SupervisorControl.NextAction != "stop_and_report" {
		t.Fatalf("readiness failure was not exposed as unavailable: %+v", result.response.SupervisorControl)
	}
}

func TestLegacySupervisorStatusKeepsHealthySupervisorAtNoAction(t *testing.T) {
	response := newResponse("supervisor")
	response.SupervisorControl = &SupervisorControlResponse{}

	result, done := legacySupervisorStateResult(
		response,
		inspection.GroupRequestRecord{},
		false,
		true,
		true,
		nil,
		false,
	)
	if !done || result.code != 0 {
		t.Fatalf("healthy supervisor status was not complete: done=%v result=%+v", done, result)
	}
	control := result.response.SupervisorControl
	if control.State != "ready" || control.LegacyReachability != "ready" || control.NextAction != "none" {
		t.Fatalf("healthy supervisor status requested migration: %+v", control)
	}
}

func TestLegacySupervisorStatusAdoptsLiveProvenanceDrift(t *testing.T) {
	response := newResponse("supervisor")
	response.SupervisorControl = &SupervisorControlResponse{}

	result, done := legacySupervisorStateResult(
		response,
		inspection.GroupRequestRecord{},
		false,
		true,
		true,
		nil,
		true,
	)
	if !done || result.code != 0 {
		t.Fatalf("live provenance drift was not adopted: done=%v result=%+v", done, result)
	}
	control := result.response.SupervisorControl
	if control.State != "ready" || control.LegacyReachability != "ready" || control.MigrationPhase != "adopted" || control.NextAction != "none" {
		t.Fatalf("live provenance drift requested an unnecessary migration: %+v", control)
	}
}

func TestLegacySupervisorStatusReportsCompletedLegacyAfterMigration(t *testing.T) {
	response := newResponse("supervisor")
	response.SupervisorControl = &SupervisorControlResponse{}

	result, done := legacySupervisorStateResult(
		response,
		inspection.GroupRequestRecord{},
		true,
		true,
		true,
		nil,
		false,
	)
	if !done || result.code != 0 {
		t.Fatalf("completed migration status was not complete: done=%v result=%+v", done, result)
	}
	control := result.response.SupervisorControl
	if control.State != "migrated" || control.LegacyReachability != "completed" || control.NextAction != "none" {
		t.Fatalf("completed legacy status was misreported: %+v", control)
	}
}

func TestLegacyEndedDelegateJobRequiresPublicationEvidence(t *testing.T) {
	store := newBareInspectionStore(t)
	supervisor, _ := newBlockingInspectionSupervisor(t, store.RootID)
	legacy := inspection.GroupRequestRecord{
		RootID: store.RootID, Supervisor: supervisor.Binding(),
		Name: (pueue.InspectionIdentity{RootID: store.RootID}).Group(), ParallelTasks: 1,
	}
	job := pueue.Job{
		ID:    7,
		State: pueue.StateEnded,
	}
	label := "delegate:" + store.RootID + ":" + strings.Repeat("a", 32)
	job.Label = &label
	if err := inspectLegacyEndedJob(context.Background(), store, legacy, job); !errors.Is(err, inspection.ErrGroupMigrationRequired) {
		t.Fatalf("ended Delegate job without publication evidence was accepted: %v", err)
	}
}

func TestLegacyEndedInspectionJobUsesInspectionJournalEvidence(t *testing.T) {
	store := newBareInspectionStore(t)
	request := newBareInspectionRequest(t, store)
	request.TaskID = strings.Repeat("b", 32)
	supervisor, _ := newBlockingInspectionSupervisor(t, store.RootID)
	digest := strings.Repeat("a", 64)
	openedAt := time.Unix(100, 0).UTC()
	operation, err := inspection.OpenOperation(store, request, inspection.Binding{
		DefinitionRevision: "inspection-v1",
		DefinitionSHA256:   digest,
		HelperExecutable:   "/usr/bin/true",
		HelperSHA256:       digest,
		WorkerExecutable:   "/usr/bin/true",
		WorkerSHA256:       digest,
		Supervisor:         supervisor.Binding(),
	}, openedAt)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := operation.ClaimSubmission(openedAt.Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err = submission.ConsumeInspectionSubmission(); err != nil {
		t.Fatal(err)
	}
	start, err := operation.ClaimStart(openedAt.Add(2 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err = start.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = operation.Complete(inspection.ResultUnavailable, nil, openedAt.Add(3*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err = operation.RecordReceipt(7); err != nil {
		t.Fatal(err)
	}
	if err = operation.Close(); err != nil {
		t.Fatal(err)
	}

	job := pueue.Job{ID: 7, State: pueue.StateEnded}
	label := (pueue.InspectionIdentity{RootID: store.RootID, TaskID: request.TaskID}).Label()
	job.Label = &label
	job.Group = (pueue.InspectionIdentity{RootID: store.RootID}).Group()
	legacy := inspection.GroupRequestRecord{
		RootID: store.RootID, Supervisor: supervisor.Binding(),
		Name: job.Group, ParallelTasks: 1,
	}
	if err = prepareEndedInspectionEvidence(t.Context(), store, legacy, request.TaskID, job); err != nil {
		t.Fatal(err)
	}
	if err = inspectLegacyEndedJob(context.Background(), store, legacy, job); err != nil {
		t.Fatalf("completed inspection was not accepted from its journal: %v", err)
	}

	assertPrivateCleanupSelectsOnlyProvenRows(t, store, supervisor.Binding(), job)

	wrongGroup := legacy
	wrongGroup.Name = "default"
	if err = inspectLegacyEndedJob(context.Background(), store, wrongGroup, job); !errors.Is(err, inspection.ErrGroupMigrationRequired) {
		t.Fatalf("inspection from a different group was accepted: %v", err)
	}
	wrongSupervisor := legacy
	wrongSupervisor.Supervisor.Endpoint += ".other"
	if err = inspectLegacyEndedJob(context.Background(), store, wrongSupervisor, job); !errors.Is(err, inspection.ErrGroupMigrationRequired) {
		t.Fatalf("inspection from a different supervisor was accepted: %v", err)
	}
}

func TestSupervisorMigrateDoesNotInitializeMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	result := supervisorMigrate(root, Dependencies{}, newSupervisorTestResponse())
	if result.code != 0 || result.err != nil {
		t.Fatalf("missing root was not a read-only no-op: code=%d err=%v response=%+v", result.code, result.err, result.response.SupervisorControl)
	}
	control := result.response.SupervisorControl
	if control == nil || control.State != "not_initialized" || control.LegacyReachability != "not_present" || control.ActiveWork != "none" || control.NextAction != "none" {
		t.Fatalf("unexpected missing-root migration response: %+v", control)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing root was created by migration: %v", err)
	}
}

func TestSupervisorMigrateDoesNotCreateInspectionGroupWithoutLegacyJournal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	result := supervisorMigrate(root, Dependencies{}, newSupervisorTestResponse())
	if result.code != 0 || result.err != nil {
		t.Fatalf("root without legacy journal was not a read-only no-op: code=%d err=%v response=%+v", result.code, result.err, result.response.SupervisorControl)
	}
	control := result.response.SupervisorControl
	if control == nil || control.State != "not_initialized" || control.LegacyReachability != "not_present" || control.ActiveWork != "none" || control.NextAction != "none" {
		t.Fatalf("unexpected no-journal migration response: %+v", control)
	}
	if _, err := os.Stat(filepath.Join(root, ".supervisor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("migration created private supervisor state without a legacy journal: %v", err)
	}
}

func newSupervisorTestResponse() Response {
	response := newResponse("supervisor")
	response.SupervisorControl = &SupervisorControlResponse{
		Action:             "migrate",
		State:              "unknown",
		LegacyReachability: "unknown",
		ActiveWork:         "unknown",
		NextAction:         "stop_and_report",
	}
	return response
}

func TestEnsureInspectionGroupHonorsBootstrapLockDeadline(t *testing.T) {
	store := newBareInspectionStore(t)
	client, _, _ := newUncertainGroupSupervisorWithConfig(t, store.RootID, pueue.PrivateConfigPath(store.Root))
	locked := make(chan struct{})
	release := make(chan struct{})
	lockErr := make(chan error, 1)
	go func() {
		lockErr <- pueue.WithBootstrapLock(context.Background(), store.Root, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	err := supervisorcore.EnsureInspectionGroup(store, client, 25*time.Millisecond)
	close(release)
	if lockResult := <-lockErr; lockResult != nil {
		t.Fatalf("bootstrap lock holder failed: %v", lockResult)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bootstrap lock wait was not bounded: %v", err)
	}
}

func TestRebindContinuationSupervisorHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	saved := task.SupervisorRef{ConfigPath: pueue.PrivateConfigPath("/state")}
	_, err := rebindContinuationSupervisor(ctx, "/state", saved, Dependencies{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled continuation rebind was not bounded: %v", err)
	}
}

func TestRebindContinuationSupervisorUsesPublishedSuccessor(t *testing.T) {
	store := newBareInspectionStore(t)
	digest := strings.Repeat("a", 64)
	legacy := admissionfixture.ContinuationSupervisorRef(store.Root, digest, "unix:/legacy.sock")
	successor := admissionfixture.ContinuationSupervisorRef(store.Root, strings.Repeat("b", 64), "unix:/successor.sock")
	legacyGroup := inspection.GroupRequestRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        store.RootID,
		Supervisor:    legacy,
		Name:          (pueue.InspectionIdentity{RootID: store.RootID}).Group(),
		ParallelTasks: 1,
	}
	if group, err := inspection.OpenGroup(store, legacy); err != nil {
		t.Fatal(err)
	} else if err = group.Close(); err != nil {
		t.Fatal(err)
	}
	successorGroup := legacyGroup
	successorGroup.Supervisor = successor
	publishAppTestGroupMigration(t, store, legacyGroup, successorGroup)

	got, err := rebindContinuationSupervisor(context.Background(), store.Root, legacy, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	if !task.SameSupervisorIdentity(got, successor) {
		t.Fatalf("continuation kept retired supervisor: got=%+v want=%+v", got, successor)
	}
}

func TestRebindContinuationSupervisorRejectsPrivateEndpointDriftWithoutInspectionJournal(t *testing.T) {
	rawRoot, err := os.MkdirTemp("/tmp", "dl-rebind-")
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(rawRoot)
	if err != nil {
		t.Fatal(err)
	}
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if removeErr := os.RemoveAll(rawRoot); removeErr != nil {
			t.Error(removeErr)
		}
	})
	current, logPath, _ := newUncertainGroupSupervisorWithConfig(t, store.RootID, pueue.PrivateConfigPath(store.Root))
	clientPath := current.Binding().ClientExecutable
	daemonPath := filepath.Join(filepath.Dir(clientPath), "pueued")
	if err = os.WriteFile(daemonPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	privateBase := filepath.Dir(pueue.PrivateConfigPath(store.Root))
	config, err := json.Marshal(map[string]any{
		"shared": map[string]any{
			"pueue_directory":    filepath.Join(privateBase, "data"),
			"runtime_directory":  filepath.Join(privateBase, "run"),
			"unix_socket_path":   filepath.Join(privateBase, "run", "socket"),
			"alias_file":         filepath.Join(privateBase, "aliases"),
			"pid_path":           filepath.Join(privateBase, "run", "pid"),
			"shared_secret_path": filepath.Join(privateBase, "data", "secret"),
			"daemon_cert":        filepath.Join(privateBase, "data", "cert"),
			"daemon_key":         filepath.Join(privateBase, "data", "key"),
		},
		"daemon": map[string]any{"shell_command": []string{"/bin/sh", "-c", "{{ pueue_command_string }}"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(pueue.PrivateConfigPath(store.Root), config, 0o600); err != nil {
		t.Fatal(err)
	}
	statusPath := filepath.Join(filepath.Dir(logPath), "status.json")
	options := pueue.Options{
		Environment:        append(os.Environ(), "FAKE_LOG="+logPath, "FAKE_STATUS="+statusPath),
		ObservationTimeout: time.Second,
	}
	saved := current.Binding()
	saved.ConfigDigest = strings.Repeat("a", 64)
	saved.ResolvedConfigSHA256 = strings.Repeat("b", 64)

	_, err = rebindContinuationSupervisor(context.Background(), store.Root, saved, Dependencies{
		InitialSupervisorExecutable: clientPath,
		SupervisorOptions:           options,
	})
	if !errors.Is(err, pueue.ErrPrivateSupervisorMigrationRequired) {
		t.Fatalf("continuation accepted private endpoint drift without a migration journal: %v", err)
	}
}

func assertPrivateCleanupSelectsOnlyProvenRows(t *testing.T, store *taskdir.Store, binding task.SupervisorRef, job pueue.Job) {
	t.Helper()
	cleanupJob := job
	activeLabel := "delegate:" + store.RootID + ":" + strings.Repeat("c", 32)
	unknownLabel := "delegate:" + store.RootID + ":" + strings.Repeat("d", 32)
	candidates := []pueue.Job{
		cleanupJob,
		{ID: 8, Label: &activeLabel, State: pueue.StateRunning, Group: "default"},
		{ID: 9, Label: &unknownLabel, State: pueue.StateEnded, Succeeded: true, Group: "default"},
	}
	plan, cleanupErr := authorizePrivateSuccessfulCleanup(context.Background(), store.Root, binding, candidates)
	if cleanupErr != nil || len(plan.RemoveIDs) != 1 || plan.RemoveIDs[0] != job.ID || plan.Settling {
		t.Fatalf("unrelated active/incomplete tasks blocked exact cleanup: plan=%+v err=%v", plan, cleanupErr)
	}

	duplicate := cleanupJob
	duplicate.ID = 10
	plan, cleanupErr = authorizePrivateSuccessfulCleanup(context.Background(), store.Root, binding, append(candidates, duplicate))
	if cleanupErr != nil || len(plan.RemoveIDs) != 0 || plan.Settling {
		t.Fatalf("duplicate label was authorized: plan=%+v err=%v", plan, cleanupErr)
	}
}
