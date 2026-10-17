package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestDispatchExistingIDConflictStopsBeforeAdmission(t *testing.T) {
	store, td, req := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	briefPath := filepath.Join(t.TempDir(), "changed.md")
	writeAppTestFile(t, briefPath, []byte("changed brief"))

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"dispatch", "--json", "--root", store.Root, "--id", req.TaskID,
		"--pueue-config", filepath.Join(t.TempDir(), "missing.yml"),
		"--provider", req.Provider, "--brief", briefPath, "--cwd", req.CanonicalCwd,
		"--runner", filepath.Join(t.TempDir(), "runner"),
	}, &stdout, &stderr, Dependencies{InitialSupervisorExecutable: filepath.Join(t.TempDir(), "missing-pueue")})
	if code != 2 {
		t.Fatalf("conflict exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.TaskRecord != "created" || response.Failure == nil ||
		response.Failure.Code != "task_request_conflict" ||
		response.Failure.Stage != "existing-task" ||
		response.Failure.NextAction != "check_status" {
		t.Fatalf("existing identity conflict was not classified safely: %+v", response)
	}
	if _, err := td.ReadSubmission(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conflicting dispatch admitted existing task: %v", err)
	}
}

func TestDispatchValidationFailureKeepsExplicitTaskPresenceUnknown(t *testing.T) {
	store, td, req := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"dispatch", "--json", "--root", store.Root, "--id", req.TaskID,
		"--provider", req.Provider, "--brief", filepath.Join(t.TempDir(), "missing.md"),
		"--cwd", req.CanonicalCwd,
	}, &stdout, &stderr, Dependencies{})
	if code != 2 {
		t.Fatalf("invalid dispatch exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.TaskID != req.TaskID || response.TaskRecord != "unknown" || response.Failure == nil ||
		response.Failure.Code != "invalid_request" ||
		response.Failure.Stage != "validate-request" ||
		response.Failure.NextAction != "stop_and_report" {
		var failure DispatchFailureResponse
		if response.Failure != nil {
			failure = *response.Failure
		}
		t.Fatalf("validation failure misreported an explicit task ID: task_id=%q task_record=%q failure=%+v",
			response.TaskID, response.TaskRecord, failure)
	}
}

func TestDispatchGeneratedIDValidationFailureRemainsCorrectable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	cwd := t.TempDir()
	brief := filepath.Join(t.TempDir(), "brief.md")
	writeAppTestFile(t, brief, []byte("dispatch brief"))
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"dispatch", "--json", "--root", root, "--provider", "antigravity:print",
		"--brief", brief, "--cwd", cwd, "--budget", "2m", "--native-timeout", "5m",
	}, &stdout, &stderr, Dependencies{})
	if code != 2 {
		t.Fatalf("invalid generated-ID dispatch exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.TaskID == "" || response.TaskRecord != "not_created" || response.Failure == nil ||
		response.Failure.Code != "invalid_request" ||
		response.Failure.Stage != "validate-request" ||
		response.Failure.NextAction != "correct_request" {
		var failure DispatchFailureResponse
		if response.Failure != nil {
			failure = *response.Failure
		}
		t.Fatalf("generated task ID validation failure was not safely correctable: task_id=%q task_record=%q failure=%+v",
			response.TaskID, response.TaskRecord, failure)
	}
}

func TestRunnerArgumentValidationRejectsProviderArguments(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	id := strings.Repeat("b", 32)
	for _, args := range [][]string{
		{"--root", root, id, "provider-argv"},
		{"--root", root, "--json", id, id},
		{"--root", root + "/..", id},
	} {
		if _, err := parseRunnerArguments(args); err == nil {
			t.Fatalf("runner accepted unsafe arguments: %q", args)
		}
	}
	parsed, err := parseRunnerArguments([]string{"--root", root, "--inspection", "--supervised", id})
	if err != nil || !parsed.Inspection || !parsed.Supervised || parsed.TaskID != id {
		t.Fatalf("runner rejected supervised inspection arguments: parsed=%+v err=%v", parsed, err)
	}
	maintenance, err := parseRunnerArguments([]string{"--queue-maintenance", "--root", root})
	if err != nil || !maintenance.QueueMaintenance || maintenance.TaskID != "" {
		t.Fatalf("runner rejected queue maintenance arguments: parsed=%+v err=%v", maintenance, err)
	}
	for _, args := range [][]string{
		{"--queue-maintenance", "--root", root, id},
		{"--queue-maintenance", "--terminal-event", "--root", root, "--pueue-id", "7", "--exit-code", "0"},
		{"--queue-maintenance", "--json", "--root", root},
	} {
		if _, err = parseRunnerArguments(args); err == nil {
			t.Fatalf("runner accepted mixed queue maintenance arguments: %q", args)
		}
	}
}

func TestStatusTerminalWinnerSkipsSupervisorReconciliation(t *testing.T) {
	store, td, req := newAppTestTask(t, true)
	defer closeAppTestTask(t, store, td)
	if _, cleanupErr, collectErr := td.Collect(task.FixturePredicateRef()); cleanupErr != nil || collectErr != nil {
		t.Fatalf("preparing terminal winner cleanup=%v collect=%v", cleanupErr, collectErr)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"status", req.TaskID, "--root", store.Root, "--json"}, &stdout, &stderr, Dependencies{SupervisorOptions: pueue.Options{Observer: func(pueue.CommandEvent) { t.Fatal("terminal status invoked supervisor") }}})
	if code != 0 {
		t.Fatalf("terminal status exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Publication != task.PublicationCommitted.String() || response.Outcome == nil {
		t.Fatalf("terminal winner was not preserved: %+v", response)
	}
}

func newAppTestTask(t *testing.T, sealed bool) (*taskdir.Store, *taskdir.TaskDir, *task.TaskRecord) {
	t.Helper()
	return lifecyclefixture.NewAppTestTask(t, sealed)
}

func newAppTestTaskWithPrior(t *testing.T, sealed bool, prior *task.PriorSession) (*taskdir.Store, *taskdir.TaskDir, *task.TaskRecord) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	td, req := newAppTaskInStore(t, store, strings.Repeat("c", 32), sealed, prior)
	return store, td, req
}

func newAppTaskInStore(t *testing.T, store *taskdir.Store, id string, sealed bool, prior *task.PriorSession) (*taskdir.TaskDir, *task.TaskRecord) {
	t.Helper()
	return lifecyclefixture.NewAppTaskInStore(t, store, id, sealed, prior)
}

func newAppTaskInStoreWithSupervisorConfig(t *testing.T, store *taskdir.Store, id string, sealed bool, prior *task.PriorSession, supervisorConfig string) (*taskdir.TaskDir, *task.TaskRecord) {
	t.Helper()
	return lifecyclefixture.NewAppTaskInStoreWithSupervisorConfig(t, store, id, sealed, prior, supervisorConfig)
}

func closeAppTestTask(t *testing.T, store *taskdir.Store, td *taskdir.TaskDir) {
	t.Helper()
	lifecyclefixture.CloseAppTestTask(t, store, td)
}

func writeAppTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPiContinuationRequiresMatchingWorkspace(t *testing.T) {
	request := task.TaskRecord{Provider: config.ProviderPiJSON, Mode: config.ModeReadOnly, CanonicalCwd: "/workspace/current"}
	predecessor := request
	if err := NativeCatalog().ValidateContinuation(predecessor, task.MetaRecord{}, request); err != nil {
		t.Fatalf("same workspace rejected: %v", err)
	}
	predecessor.CanonicalCwd = "/workspace/other"
	if err := NativeCatalog().ValidateContinuation(predecessor, task.MetaRecord{}, request); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("cross-workspace Pi continuation error=%v", err)
	}
}
