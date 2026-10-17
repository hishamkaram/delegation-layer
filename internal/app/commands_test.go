package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func TestRunNativeProfileRefusesBeforeBinding(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	briefPath := filepath.Join(t.TempDir(), "brief.md")
	writeAppTestFile(t, briefPath, []byte("dispatch brief"))
	missingSupervisor := filepath.Join(t.TempDir(), "pueue")
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"dispatch", "--json", "--root", root,
		"--pueue-config", filepath.Join(t.TempDir(), "missing.yml"),
		"--provider", "codex:exec", "--brief", briefPath, "--cwd", filepath.Dir(briefPath),
		"--id", strings.Repeat("a", 32), "--runner", filepath.Join(t.TempDir(), "runner"),
	}, &stdout, &stderr, Dependencies{InitialSupervisorExecutable: missingSupervisor})
	if code != 2 {
		t.Fatalf("native refusal exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.SchemaVersion != OutputSchemaVersion || response.Command != "dispatch" {
		t.Fatalf("unexpected response identity: %+v", response)
	}
	if response.Failure == nil || response.Failure.Code != "provider_unavailable" ||
		response.Failure.Stage != "prepare-provider" || response.TaskRecord != "unknown" ||
		response.Failure.NextAction != "stop_and_report" {
		t.Fatalf("profile refusal was not expressed through the dispatch contract: %+v", response)
	}
	if strings.Contains(response.Error, ErrProfileUnavailable.Error()) {
		t.Fatalf("JSON response exposed internal provider diagnostics: %+v", response)
	}
	tasks, err := os.ReadDir(filepath.Join(root, "tasks"))
	if err == nil && len(tasks) != 0 {
		t.Fatalf("native refusal created task authority: %v", tasks)
	}
}

func TestSupervisorStatusDoesNotInitializeState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"supervisor", "status", "--root", root, "--json"}, &stdout, &stderr, Dependencies{})
	if code != 0 {
		t.Fatalf("supervisor status exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Command != "supervisor" || response.SupervisorControl == nil ||
		response.SupervisorControl.State != "not_initialized" ||
		response.SupervisorControl.LegacyReachability != "not_present" {
		t.Fatalf("unexpected supervisor status response: %+v", response)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only supervisor status initialized state root: %v", err)
	}
}

func TestSupervisorStatusTreatsStoreWithoutPrivateConfigAsNotInitialized(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := taskdir.InitStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"supervisor", "status", "--root", root, "--json"}, &stdout, &stderr, Dependencies{})
	if code != 0 {
		t.Fatalf("supervisor status exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	control := response.SupervisorControl
	if control == nil || control.State != "not_initialized" || control.LegacyReachability != "not_present" || control.ActiveWork != "none" || control.NextAction != "none" {
		t.Fatalf("store without private config was not treated as uninitialized: %+v", control)
	}
}

func TestSupervisorStatusRejectsDamagedStateRoot(t *testing.T) {
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"supervisor", "status", "--root", root, "--json"}, &stdout, &stderr, Dependencies{})
	if code == 0 {
		t.Fatalf("damaged state root was reported as healthy: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.SupervisorControl == nil || response.SupervisorControl.State == "not_initialized" {
		t.Fatalf("damaged state root was treated as absent: %+v", response)
	}
}

func TestCollectReturnsCommittedWinnerAndBoundedResponse(t *testing.T) {
	store, td, req := newAppTestTask(t, true)
	defer closeAppTestTask(t, store, td)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"collect", req.TaskID, "--root", store.Root, "--json"}, &stdout, &stderr, Dependencies{})
	if code != 0 {
		t.Fatalf("collect exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Publication != task.PublicationCommitted.String() || response.Outcome == nil || response.Payload == nil {
		t.Fatalf("committed publication missing: %+v", response)
	}
	if response.Payload.Basename != "result.txt" || response.Payload.Length != int64(len("answer\n")) {
		t.Fatalf("unexpected payload descriptor: %+v", response.Payload)
	}
	if response.Raw != nil {
		t.Fatalf("collect copied raw output into control response: %+v", response.Raw)
	}
	if stdout.Len() > 16*1024 {
		t.Fatalf("control response is unexpectedly large: %d", stdout.Len())
	}
}

func TestCollectPendingDoesNotAcquireSupervisorAuthority(t *testing.T) {
	store, td, req := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"collect", req.TaskID, "--root", store.Root, "--json"}, &stdout, &stderr, Dependencies{})
	if code != 3 {
		t.Fatalf("pending collect exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Pending == nil || response.Pending.Kind != "publication" || response.Admission != task.AdmissionUnknown.String() {
		t.Fatalf("pending publication was not explicit: %+v", response)
	}
	if len(response.Raw) != 2 {
		t.Fatalf("pending publication omitted raw descriptors: %+v", response.Raw)
	}
	for i, name := range []string{"stderr", "stdout"} {
		descriptor := response.Raw[i]
		if descriptor.Path != filepath.Join(td.Dir, "raw", name) || descriptor.Sealed || descriptor.Size != nil || descriptor.SHA256 != "" {
			t.Fatalf("pending publication returned an invalid live descriptor: %+v", descriptor)
		}
	}
	if _, err := td.ReadSubmission(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collect acquired submission authority: %v", err)
	}
}

func TestCollectWithoutBudgetStopDoesNotRecoverPrivateSupervisor(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	td, req := newAppTaskInStoreWithSupervisorConfig(t, store, strings.Repeat("d", 32), false, nil, pueue.PrivateConfigPath(store.Root))
	defer closeAppTestTask(t, store, td)

	result := collect(Arguments{Root: store.Root, TaskID: req.TaskID}, storeDependencies{
		observeSupervisor:           true,
		initialSupervisorExecutable: filepath.Join(t.TempDir(), "missing-pueue"),
	})
	if result.code != 3 || result.err != nil {
		t.Fatalf("collect without budget-stop evidence tried supervisor recovery: %+v", result)
	}
	if _, err = os.Lstat(filepath.Join(store.Root, ".supervisor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collect without budget-stop evidence created private supervisor state: %v", err)
	}
}

func TestCancelWithoutSubmissionDoesNotRecoverPrivateSupervisor(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	td, req := newAppTaskInStoreWithSupervisorConfig(t, store, strings.Repeat("e", 32), false, nil, pueue.PrivateConfigPath(store.Root))
	defer closeAppTestTask(t, store, td)

	result := cancel(Arguments{Root: store.Root, TaskID: req.TaskID}, Dependencies{
		InitialSupervisorExecutable: filepath.Join(t.TempDir(), "missing-pueue"),
	})
	if result.err != nil || result.code != 0 || result.response.Stop == nil || result.response.Stop.Action != "not-launched" || result.response.Stop.NumericTaskID != nil {
		t.Fatalf("cancel prepared task without supervisor recovery: %+v", result)
	}
	if _, err = os.Lstat(filepath.Join(store.Root, ".supervisor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancel without submission acquired supervisor authority: %v", err)
	}
}

type cancelQueueRepairFixture struct {
	initialExecutable string
	commandLog        string
	statusPath        string
	options           pueue.Options
	supervisor        *pueue.Client
}

func newCancelQueueRepairFixture(t *testing.T, stateRoot string) cancelQueueRepairFixture {
	t.Helper()
	supervisorDir := filepath.Join(stateRoot, ".supervisor")
	if err := os.MkdirAll(supervisorDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bundleDir := t.TempDir()
	clientPath := filepath.Join(bundleDir, "pueue")
	daemonPath := filepath.Join(bundleDir, "pueued")
	initialExecutable := filepath.Join(bundleDir, "delegate")
	commandLog := filepath.Join(t.TempDir(), "pueue-commands.log")
	statusPath := filepath.Join(t.TempDir(), "pueue-status.json")
	writeRunnerFixture(t, clientPath, "#!/bin/sh\nset -eu\ncommand=\nfor arg in \"$@\"; do\n  case \"$arg\" in\n    --version|status|parallel|remove|kill) command=$arg ;;\n  esac\ndone\nprintf '%s\\n' \"$command\" >> \"$FAKE_COMMAND_LOG\"\ncase \"$command\" in\n  --version) printf '%s\\n' 'pueue 4.0.4' ;;\n  status) cat \"$FAKE_STATUS_PATH\" ;;\n  parallel) ;;\n  remove|kill) ;;\n  *) exit 64 ;;\nesac\n")
	writeRunnerFixture(t, daemonPath, "#!/bin/sh\nexit 0\n")
	writeRunnerFixture(t, initialExecutable, "#!/bin/sh\nexit 0\n")
	configPath := pueue.PrivateConfigPath(stateRoot)
	configData, err := json.Marshal(map[string]any{"shared": map[string]string{
		"pueue_directory":    filepath.Join(supervisorDir, "data"),
		"runtime_directory":  filepath.Join(supervisorDir, "runtime"),
		"unix_socket_path":   filepath.Join(supervisorDir, "runtime", "pueue.sock"),
		"alias_file":         filepath.Join(supervisorDir, "aliases"),
		"pid_path":           filepath.Join(supervisorDir, "runtime", "pueue.pid"),
		"shared_secret_path": filepath.Join(supervisorDir, "secret"),
		"daemon_cert":        filepath.Join(supervisorDir, "cert"),
		"daemon_key":         filepath.Join(supervisorDir, "key"),
	}, "daemon": map[string]any{
		"shell_command": []string{"/bin/sh", "-c", "{{ pueue_command_string }}"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	options := pueue.Options{
		// This test exercises cancellation while optional queue repair is busy.
		// Use the production observation window so scheduler contention under
		// -race cannot turn that separate invariant into a false failure.
		ObservationTimeout: pueue.DefaultObservationTimeout,
		Environment: append(pueue.DefaultEnvironment(),
			"FAKE_COMMAND_LOG="+commandLog, "FAKE_STATUS_PATH="+statusPath),
	}
	supervisor, err := pueue.Bind(context.Background(), clientPath, configPath, options)
	if err != nil {
		t.Fatal(err)
	}
	return cancelQueueRepairFixture{
		initialExecutable: initialExecutable,
		commandLog:        commandLog,
		statusPath:        statusPath,
		options:           options,
		supervisor:        supervisor,
	}
}

func createQueuedCancelTask(t *testing.T, stateRoot, statusPath string, supervisor *pueue.Client) string {
	t.Helper()
	store, err := taskdir.InitStore(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	runnerSource := canonicalAppTestExecutable(t)
	runner := installStateRunnerFixture(t, stateRoot, runnerSource)
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	taskID := strings.Repeat("f", 32)
	brief := []byte("cancel should reach the supervisor even if queue repair is not available")
	digest := task.ComputeSHA256([]byte("cancel test"))
	requested := task.TaskConfig{Permission: "read-only", Budget: "1m0s"}
	req := &task.TaskRecord{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: taskID,
		Provider: "fixture:test", Mode: "read-only", CanonicalCwd: workspace,
		RequestedConfig: requested, BudgetNanos: int64(time.Minute),
		BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief)),
	}
	meta := &task.MetaRecord{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: taskID,
		RequestedConfig: requested,
		EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: digest},
		Containment:     "fixture-only", Approval: "never", ProviderExecutable: "/tmp/app-provider",
		ProviderVersion: "fixture-v2", PublisherBuild: "app-test", PublisherVersion: "app-test",
		Predicate: task.FixturePredicateRef(), SupervisorConfig: supervisor.Binding(),
		RunnerExecutable: runner, RunnerOwnership: task.RunnerOwnershipManaged,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	td, err := store.CreateTask(taskID, req, brief, meta)
	if err != nil {
		t.Fatal(err)
	}
	submissionPermit, err := td.PrepareSubmission(supervisor.Binding())
	if err != nil {
		t.Fatal(err)
	}
	if err = submissionPermit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = submissionPermit.Release(); err != nil {
		t.Fatal(err)
	}
	submission, err := td.ReadSubmission()
	if err != nil {
		t.Fatal(err)
	}
	writeQueuedCancelStatus(t, statusPath, submission.Label, workspace)
	targetID := int64(7)
	observation, err := supervisor.Reconcile(context.Background(), pueue.Identity{
		RootID: submission.RootID, TaskID: submission.TaskID,
		SpecSHA256: submission.SpecSHA256, MetaSHA256: submission.MetaSHA256,
		NumericTaskID: &targetID,
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := observation.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	if err = td.RecordSupervisorReceipt(*receipt); err != nil {
		t.Fatal(err)
	}
	if err = td.Close(); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	return taskID
}

func writeQueuedCancelStatus(t *testing.T, path, label, workspace string) {
	t.Helper()
	const timestamp = "2026-09-13T20:00:00.123456789Z"
	data, err := json.Marshal(map[string]any{
		"tasks": map[string]any{"7": map[string]any{
			"id": int64(7), "created_at": timestamp,
			"original_command": "an unrelated queued command", "command": "an unrelated queued command",
			"path": workspace, "envs": map[string]string{}, "group": "default",
			"dependencies": []int64{}, "priority": int32(0), "label": label,
			"status": map[string]any{"Queued": map[string]string{"enqueued_at": timestamp}},
		}},
		"groups": map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCancelStopsTaskWithoutRunningOptionalQueueRepair(t *testing.T) {
	stateRoot := testsupport.CanonicalTempDir(t)
	store, err := taskdir.InitStore(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	fixture := newCancelQueueRepairFixture(t, stateRoot)
	taskID := createQueuedCancelTask(t, stateRoot, fixture.statusPath, fixture.supervisor)

	maintenanceLease, err := taskdir.OpenLockFile(filepath.Join(stateRoot, ".maintenance.lock"), taskdir.LockLevelMaintenance)
	if err != nil {
		t.Fatal(err)
	}
	if err = maintenanceLease.LockSH(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := maintenanceLease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	result := cancel(Arguments{Root: stateRoot, TaskID: taskID}, Dependencies{
		SupervisorOptions: fixture.options, InitialSupervisorExecutable: fixture.initialExecutable,
	})
	if result.code != 0 || result.err != nil || result.response.Stop == nil || result.response.Stop.Action != "remove" {
		t.Fatalf("cancel did not send the saved stop request while holding a shared task lease: %+v", result)
	}
	commands, err := os.ReadFile(fixture.commandLog)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(commands), "remove\n"); got != 1 {
		t.Fatalf("supervisor received remove %d times, want exactly once; commands=%q", got, commands)
	}
	if _, err = os.Lstat(filepath.Join(stateRoot, ".supervisor", "pueue-pair.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancel performed optional supervisor-pair migration: %v", err)
	}
}

func activeLifetimeOwnerCancelFixture(t *testing.T) (string, cancelQueueRepairFixture, string, *taskdir.TaskDir) {
	t.Helper()
	stateRoot := testsupport.CanonicalTempDir(t)
	store, err := taskdir.InitStore(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	fixture := newCancelQueueRepairFixture(t, stateRoot)
	taskID := createQueuedCancelTask(t, stateRoot, fixture.statusPath, fixture.supervisor)

	ownerStore, err := taskdir.OpenStore(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	ownerTask, err := ownerStore.OpenTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := ownerTask.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
		if closeErr := ownerTask.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if closeErr := ownerStore.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return stateRoot, fixture, taskID, ownerTask
}

func TestCancelRoutesThroughActiveLifetimeOwnerWithoutKillingSupervisorJob(t *testing.T) {
	stateRoot, fixture, taskID, ownerTask := activeLifetimeOwnerCancelFixture(t)
	result := cancel(Arguments{Root: stateRoot, TaskID: taskID}, Dependencies{
		SupervisorOptions: fixture.options, InitialSupervisorExecutable: fixture.initialExecutable,
	})
	if result.code != 0 || result.err != nil || result.response.Stop == nil {
		t.Fatalf("cancel did not durably route through the lifetime owner: %+v", result)
	}
	stop := result.response.Stop
	if stop.Action != "terminate" || !stop.Requested || !stop.Attempted || !stop.InFlight || stop.Terminated {
		t.Fatalf("unexpected lifetime-owned stop response: %+v", stop)
	}
	commands, err := os.ReadFile(fixture.commandLog)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(commands); strings.Contains(got, "remove\n") || strings.Contains(got, "kill\n") {
		t.Fatalf("cancel bypassed the active lifetime owner: %q", got)
	}
	requested, err := ownerTask.UserStopRequested()
	if err != nil || !requested {
		t.Fatalf("durable user stop requested = %t, error = %v", requested, err)
	}
}

func TestCollectPredicateFailuresRemainOperationalErrors(t *testing.T) {
	tests := []struct {
		name   string
		deps   Dependencies
		wantIn error
	}{
		{
			name: "unknown registry",
			deps: Dependencies{PredicateRegistry: func() predicate.Registry {
				registry, err := predicate.New()
				if err != nil {
					panic(err)
				}
				return registry
			}},
			wantIn: task.ErrIncompatiblePredicate,
		},
		{
			name: "interpreter failure",
			deps: Dependencies{PredicateRegistry: func() predicate.Registry {
				registry, err := predicate.New(failingInterpreter{err: errInjectedPredicate})
				if err != nil {
					panic(err)
				}
				return registry
			}},
			wantIn: errInjectedPredicate,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, td, req := newAppTestTask(t, true)
			defer closeAppTestTask(t, store, td)

			var stdout, stderr bytes.Buffer
			code := Run([]string{"collect", req.TaskID, "--root", store.Root, "--json"}, &stdout, &stderr, test.deps)
			if code != 1 {
				t.Fatalf("collect exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			var response Response
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(response.Error, test.wantIn.Error()) {
				t.Fatalf("predicate failure missing from response: %+v", response)
			}
			if response.Publication != task.PublicationPending.String() {
				t.Fatalf("collect lost independently observed pending state: %+v", response)
			}
		})
	}
}

func TestCollectionInspectionWinnerUsesVerdictAfterPendingRace(t *testing.T) {
	cases := []struct {
		name        string
		verdict     string
		publication task.Publication
		code        int
	}{
		{name: "committed", verdict: task.VerdictCommitted, publication: task.PublicationCommitted, code: 0},
		{name: "rejected", verdict: task.VerdictRejected, publication: task.PublicationRejected, code: 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := newResponse("collect")
			outcome := &task.OutcomeRecord{Verdict: tc.verdict, Payload: task.PayloadDescriptor{Basename: "result.txt"}}
			inspection := &taskdir.TaskInspection{Publication: tc.publication, Outcome: outcome}
			winner, collectErr, handled := tasklifecycle.CollectionInspectionOutcome(inspection, errors.Join(task.ErrNoSeal, task.ErrLockBusy), nil, nil)
			if !handled || winner == nil {
				t.Fatalf("pending race lost terminal inspection: handled=%t winner=%+v err=%v", handled, winner, collectErr)
			}
			result := collectionResult(response, winner, collectErr)
			if !handled || result.code != tc.code || result.err != nil || result.response.Outcome != outcome {
				t.Fatalf("pending race lost terminal verdict: handled=%t result=%+v", handled, result)
			}
		})
	}
	infra := errors.New("inspection storage failed")
	response := newResponse("collect")
	outcome := &task.OutcomeRecord{Verdict: task.VerdictRejected, Payload: task.PayloadDescriptor{Basename: "publish.reject"}}
	inspection := &taskdir.TaskInspection{Publication: task.PublicationRejected, Outcome: outcome}
	winner, collectErr, handled := tasklifecycle.CollectionInspectionOutcome(inspection, errors.Join(task.ErrNoSeal, infra), nil, nil)
	if !handled || winner == nil {
		t.Fatalf("infrastructure fault lost terminal inspection: handled=%t winner=%+v err=%v", handled, winner, collectErr)
	}
	result := collectionResult(response, winner, collectErr)
	if !handled || result.code != 1 || !errors.Is(result.err, infra) || result.response.Outcome != outcome {
		t.Fatalf("joined infrastructure fault was downgraded: handled=%t result=%+v", handled, result)
	}
}

func TestLogsReturnsLiveAndSealedDescriptors(t *testing.T) {
	t.Run("live", func(t *testing.T) { assertLogDescriptors(t, false) })
	t.Run("sealed", func(t *testing.T) { assertLogDescriptors(t, true) })
}

func TestRunNonJSONEmitsTaskIdentityAndDescriptors(t *testing.T) {
	store, td, req := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"logs", req.TaskID, "--root", store.Root}, &stdout, &stderr, Dependencies{})
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("non-JSON logs failed: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "task_id="+req.TaskID) || !strings.Contains(output, "publication=unknown") || !strings.Contains(output, "raw path="+filepath.Join(td.Dir, "raw", "stderr")) {
		t.Fatalf("non-JSON result omitted task identity or descriptors: %q", output)
	}
}

func TestWriteHumanResponsePropagatesWriterFailure(t *testing.T) {
	errWrite := errors.New("human output failed")
	if err := writeHumanResponse(errorWriter{err: errWrite}, newResponse("status")); !errors.Is(err, errWrite) {
		t.Fatalf("human writer error=%v", err)
	}
}

func assertLogDescriptors(t *testing.T, sealed bool) {
	t.Helper()
	store, td, req := newAppTestTask(t, sealed)
	defer closeAppTestTask(t, store, td)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"logs", req.TaskID, "--root", store.Root, "--json"}, &stdout, &stderr, Dependencies{})
	if code != 0 {
		t.Fatalf("logs exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Raw) != 2 {
		t.Fatalf("unexpected descriptor count: %+v", response.Raw)
	}
	for i, name := range []string{"stderr", "stdout"} {
		descriptor := response.Raw[i]
		if descriptor.Path != filepath.Join(td.Dir, "raw", name) || descriptor.Sealed != sealed {
			t.Fatalf("wrong descriptor: %+v", descriptor)
		}
		if !sealed && (descriptor.Size != nil || descriptor.SHA256 != "") {
			t.Fatalf("live descriptor invented final identity: %+v", descriptor)
		}
		if sealed && (descriptor.Size == nil || descriptor.SHA256 == "") {
			t.Fatalf("sealed descriptor omitted final identity: %+v", descriptor)
		}
	}
}

func TestDispatchExistingRejectsChangedExplicitSupervisorConfig(t *testing.T) {
	store, td, req := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	result := dispatch(Arguments{Root: store.Root, TaskID: req.TaskID, Provider: req.Provider, Cwd: req.CanonicalCwd, Config: req.RequestedConfig, BriefBytes: []byte("brief"), PueueConfig: "/tmp/other-pueue.yml"}, Dependencies{})
	if result.code != 2 || !errors.Is(result.err, task.ErrRequestConflict) {
		t.Fatalf("changed explicit config was not refused: %+v", result)
	}
	if _, err := td.ReadSubmission(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed explicit config mutated admission: %v", err)
	}
}

func TestSameExistingRequestPreservesOmittedBudgetAcrossDefaultChanges(t *testing.T) {
	briefSHA := task.ComputeSHA256([]byte("brief"))
	existing := &task.TaskRecord{
		Provider:        config.ProviderCodexReview,
		Mode:            config.ModeReadOnly,
		CanonicalCwd:    "/workspace",
		RequestedConfig: task.TaskConfig{Permission: config.ModeReadOnly, Budget: "30m0s"},
		BudgetNanos:     int64(30 * time.Minute),
		BriefSHA256:     briefSHA,
		BriefLength:     5,
	}
	current := *existing
	current.RequestedConfig.Budget = "1h0m0s"
	current.BudgetNanos = int64(time.Hour)
	if !tasklifecycle.SameExistingRequest("", &current, existing) {
		t.Fatal("omitted retry budget did not preserve the existing request")
	}
	if tasklifecycle.SameExistingRequest("1h", &current, existing) {
		t.Fatal("explicit retry budget bypassed immutable request comparison")
	}
}

func TestDispatchExistingAcceptsMatchingExplicitSupervisorConfig(t *testing.T) {
	store, td, req := newAppTestTask(t, true)
	defer closeAppTestTask(t, store, td)
	if _, cleanupErr, collectErr := td.Collect(task.FixturePredicateRef()); cleanupErr != nil || collectErr != nil {
		t.Fatalf("preparing terminal task cleanup=%v collect=%v", cleanupErr, collectErr)
	}
	result := dispatch(Arguments{Root: store.Root, TaskID: req.TaskID, Provider: req.Provider, Cwd: req.CanonicalCwd, Config: req.RequestedConfig, BriefBytes: []byte("brief"), PueueConfig: "/tmp/app-pueue.yml"}, Dependencies{})
	if result.code != 0 || result.err != nil || result.response.Publication != task.PublicationCommitted.String() {
		t.Fatalf("matching explicit config was not accepted: %+v", result)
	}
}

func TestDispatchExistingRejectsChangedRunnerBeforePrivateQueueRepair(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	td, req := newAppTaskInStoreWithSupervisorConfig(t, store, strings.Repeat("a", 32), false, nil, pueue.PrivateConfigPath(store.Root))
	defer closeAppTestTask(t, store, td)
	result := dispatch(Arguments{Root: store.Root, TaskID: req.TaskID, Provider: req.Provider, Cwd: req.CanonicalCwd, Config: req.RequestedConfig, BriefBytes: []byte("brief"), Runner: filepath.Join(t.TempDir(), "other-runner")}, Dependencies{})
	if result.code != 2 || !errors.Is(result.err, task.ErrIdentityMismatch) {
		t.Fatalf("changed runner was not refused before supervisor repair: %+v", result)
	}
	if _, err = os.Lstat(filepath.Join(store.Root, ".supervisor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected runner override mutated private supervisor state: %v", err)
	}
}

func TestSupervisorResponseDoesNotInventUnmatchedNumericID(t *testing.T) {
	unknown := tasklifecycle.DescribeSupervisor(pueue.Observation{Matched: false, State: pueue.StateUnknown, NumericTaskID: 99})
	if unknown.NumericTaskID != nil {
		t.Fatalf("unmatched observation fabricated numeric ID: %+v", unknown)
	}
	id := int64(17)
	bound := tasklifecycle.DescribeSupervisor(pueue.Observation{Matched: false, State: pueue.StateUnknown, NumericTaskID: 99, Identity: pueue.Identity{NumericTaskID: &id}})
	if bound.NumericTaskID == nil || *bound.NumericTaskID != id {
		t.Fatalf("explicit bound ID was not retained: %+v", bound)
	}
}

func TestMergeCommandCloseMakesAllResultsOperationalFailures(t *testing.T) {
	injected := errors.New("close failed")
	for _, code := range []int{0, 2, 3, 4} {
		result := commandResult{response: newResponse("test"), code: code}
		mergeCommandClose(&result, func() error { return injected })
		if result.code != 1 || !errors.Is(result.err, injected) || !strings.Contains(result.response.Error, injected.Error()) {
			t.Fatalf("close failure code=%d was not retained: %+v", code, result)
		}
	}
}

func TestWriteJSONBoundsAndFlagsDiagnosticTruncation(t *testing.T) {
	response := newResponse("logs")
	response.RootID = strings.Repeat("a", 32)
	response.TaskID = strings.Repeat("b", 32)
	response.Outcome = &task.OutcomeRecord{SchemaVersion: task.SchemaVersion, RootID: response.RootID, TaskID: response.TaskID, Verdict: task.VerdictCommitted, Payload: task.PayloadDescriptor{Basename: "result.txt"}}
	response.Raw = []taskdir.LogDescriptor{{Path: "/tmp/raw/stdout", Available: true, Sealed: true}}
	response.Error = strings.Repeat("\\\"", MaxJSONResponseBytes)
	response.Stop = &StopResponse{Message: strings.Repeat("\\\"", MaxJSONResponseBytes)}
	response.Stops = []StopResponse{{Message: strings.Repeat("\\\"", MaxJSONResponseBytes)}}

	var output bytes.Buffer
	if err := writeJSON(&output, response); err != nil {
		t.Fatal(err)
	}
	if output.Len() > MaxJSONResponseBytes {
		t.Fatalf("JSON response exceeded bound: %d", output.Len())
	}
	var decoded Response
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatalf("response was not complete JSON: %v", err)
	}
	if !decoded.ErrorTruncated || decoded.Stop == nil || !decoded.Stop.MessageTruncated || len(decoded.Stops) != 1 || !decoded.Stops[0].MessageTruncated {
		t.Fatalf("diagnostic truncation was not explicit: %+v", decoded)
	}
	if decoded.Outcome == nil || len(decoded.Raw) != 1 {
		t.Fatalf("authority or descriptors were dropped while bounding response: %+v", decoded)
	}
}

func TestRequestHashesUseExactPreparedRecordBytes(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	metaPath := filepath.Join(td.Dir, "meta.json")
	original, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	noncanonical := append([]byte(" \n"), original...)
	if err = os.WriteFile(metaPath, noncanonical, 0o600); err != nil {
		t.Fatal(err)
	}
	_, meta, _, gotMetaHash, err := td.PreparedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := task.MarshalCanonical(meta)
	if err != nil {
		t.Fatal(err)
	}
	if gotMetaHash != task.ComputeSHA256(noncanonical) || gotMetaHash == task.ComputeSHA256(canonical) {
		t.Fatalf("request hash was reconstructed canonically: got=%s", gotMetaHash)
	}
}
