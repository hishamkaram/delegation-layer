package app

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// recordedRunnerHelper names the environment variable that carries one
// recordedRunnerRequest to the re-executed installed test binary.
const recordedRunnerHelper = "DELEGATE_TEST_RECORDED_ENVIRONMENT_RUNNER"

// recordedRunnerTimeout bounds one re-executed runner, including its provider.
const recordedRunnerTimeout = 2 * time.Minute

// recordedRunnerRequest is what the re-executed runner needs to rebuild the
// fixture's production dependencies: the case selects the catalog, and the
// remaining fields name the state root, task, fake supervisor, and result file.
type recordedRunnerRequest struct {
	Case             string `json:"case"`
	Root             string `json:"root"`
	TaskID           string `json:"task_id"`
	Runner           string `json:"runner"`
	SupervisorClient string `json:"supervisor_client"`
	CommandLog       string `json:"command_log"`
	StatusPath       string `json:"status_path"`
	Result           string `json:"result"`
}

// recordedRunnerResult is the re-executed runner's observable result.
type recordedRunnerResult struct {
	Code        int    `json:"code"`
	Err         string `json:"err"`
	Starts      int    `json:"starts"`
	Verdict     string `json:"verdict"`
	Publication string `json:"publication"`
}

// installRunner installs this test binary as the state root's protected,
// content-addressed managed runner through the production installer and
// selects it for successors only; the predecessor's records are unchanged.
func (f *recordedEnvironmentFixture) installRunner(t *testing.T) {
	t.Helper()
	installed, err := supervisor.InstallStateRunner(f.root, canonicalAppTestExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	f.deps.RunnerExecutable = installed
}

// runInstalledRunner publishes the successor's Running supervisor row and
// executes the installed runner binary once as a bounded child process, as
// the supervisor would. The child runs the production runner entry, whose
// managed-runner identity gate observes the protected installed path.
func (f *recordedEnvironmentFixture) runInstalledRunner(t *testing.T, id string) recordedRunnerResult {
	t.Helper()
	request, meta := f.loadRecords(t, id)
	if meta.RunnerOwnership != task.RunnerOwnershipManaged || meta.RunnerExecutable != f.deps.RunnerExecutable ||
		!supervisor.CurrentManagedRunnerExecutable(f.root, meta.RunnerExecutable) {
		t.Fatalf("successor runner ownership=%q is not the installed protected runner", meta.RunnerOwnership)
	}
	writeRunnerStatus(t, f.statusPath, request.RootID, id, 7)
	resultPath := filepath.Join(filepath.Dir(f.root), "runner-result.json")
	encoded, err := json.Marshal(recordedRunnerRequest{
		Case: f.testCase.name, Root: f.root, TaskID: id, Runner: f.deps.RunnerExecutable,
		SupervisorClient: f.deps.InitialSupervisorExecutable, CommandLog: f.commandLog, StatusPath: f.statusPath, Result: resultPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The child's own test deadline bounds it; CombinedOutput reaps it.
	child := exec.Command(f.deps.RunnerExecutable, "-test.run", "^TestRecordedEnvironmentRunnerHelper$", "-test.count=1",
		"-test.timeout", recordedRunnerTimeout.String())
	child.Env = append(os.Environ(), recordedRunnerHelper+"="+string(encoded))
	if output, runErr := child.CombinedOutput(); runErr != nil {
		t.Fatalf("installed runner: %v: %s", runErr, output)
	}
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	var result recordedRunnerResult
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// inspectTask returns one task's current evidence inspection.
func (f *recordedEnvironmentFixture) inspectTask(t *testing.T, id string) *taskdir.TaskInspection {
	t.Helper()
	store, err := openStore(f.root, f.deps.storeDependencies(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	td, err := store.OpenTask(id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	inspection, err := td.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	return inspection
}

// TestRecordedEnvironmentRunnerHelper is the re-executed runner. It runs only
// with a recordedRunnerRequest, rebuilds the fixture's catalog and supervisor
// dependencies, and invokes the production runner entry for one task with a
// counting Start hook.
func TestRecordedEnvironmentRunnerHelper(t *testing.T) {
	raw := os.Getenv(recordedRunnerHelper)
	if raw == "" {
		t.Skip("subprocess helper")
	}
	var request recordedRunnerRequest
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		t.Fatal(err)
	}
	testCase, found := recordedEnvironmentCaseNamed(request.Case)
	if !found {
		t.Fatalf("unknown recorded-environment case %q", request.Case)
	}
	var launches launchCounter
	deps := Dependencies{
		ResourceChecker: fixtureResources,
		Catalog:         testCase.catalog(t),
		SupervisorOptions: pueue.Options{
			Environment:        append(pueue.DefaultEnvironment(), "FAKE_COMMAND_LOG="+request.CommandLog, "FAKE_STATUS_PATH="+request.StatusPath),
			ObservationTimeout: pueue.DefaultObservationTimeout,
		},
		RunnerExecutable: request.Runner, InitialSupervisorExecutable: request.SupervisorClient,
		ExecutionHooks: launches.hooks(),
	}
	run := runRunner(runnerArguments{Root: request.Root, TaskID: request.TaskID}, deps)
	result := recordedRunnerResult{Code: run.code, Starts: launches.starts, Publication: run.response.Publication}
	if run.err != nil {
		result.Err = run.err.Error()
	}
	if run.response.Outcome != nil {
		result.Verdict = run.response.Outcome.Verdict
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(request.Result, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func recordedEnvironmentCaseNamed(name string) (recordedEnvironmentCase, bool) {
	for _, testCase := range recordedEnvironmentCases() {
		if testCase.name == name {
			return testCase, true
		}
	}
	return recordedEnvironmentCase{}, false
}
