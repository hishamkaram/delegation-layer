package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func TestRunProviderReturnsFailureForReceiptErrorWithOutcome(t *testing.T) {
	store, td, req := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)
	metaPath := filepath.Join(td.Dir, "meta.json")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta task.MetaRecord
	if err = json.Unmarshal(metaData, &meta); err != nil {
		t.Fatal(err)
	}
	providerExecutable, err := filepath.EvalSymlinks("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	meta.ProviderExecutable = providerExecutable
	metaData, err = task.MarshalCanonical(&meta)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(metaPath, metaData, 0o600); err != nil {
		t.Fatal(err)
	}
	providerBytes, err := os.ReadFile(providerExecutable)
	if err != nil {
		t.Fatal(err)
	}
	profile := PreparedProfile{
		Plan:            execution.Plan{Executable: providerExecutable, ExecutableSHA256: task.ComputeSHA256(providerBytes), Directory: req.CanonicalCwd, Predicate: task.FixturePredicateRef()},
		ObservedVersion: "fixture-v2",
		Effective:       task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: task.ComputeSHA256([]byte("app-test"))},
		Identity: func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
			return receiptErrorObserver{}, nil
		},
	}
	result := runProvider(newResponse("dispatch"), td, req, profile, nil, nil, Dependencies{PrepareProfile: func(task.TaskRecord) (PreparedProfile, error) {
		return profile, nil
	}})
	if result.code != 1 || !errors.Is(result.err, errInjectedReceipt) {
		t.Fatalf("receipt failure was not operational: %+v", result)
	}
	if result.response.Outcome == nil || result.response.Publication != task.PublicationRejected.String() {
		t.Fatalf("provider outcome was not preserved with receipt failure: %+v", result.response)
	}
}

func TestOrdinarySubmissionReceiptFailureRepairsThroughStatusWithoutResubmit(t *testing.T) {
	const numericTaskID int64 = 7

	fixture := newOrdinarySubmissionReceiptFailureFixture(t)
	receiptErr := errors.New("ordinary supervisor receipt publication failed")
	result := submitOrdinarySubmissionReceiptFailure(t, fixture, receiptErr)
	assertOrdinarySubmissionReceiptSubmission(t, fixture, result, receiptErr, numericTaskID)
	assertOrdinarySubmissionReceiptResponse(t, fixture, result)
	repairOrdinarySubmissionReceiptFailure(t, fixture, numericTaskID)
}

func TestOrdinaryStagedRecoveryAcceptsRunningRaceOnlyWithDurableReceipt(t *testing.T) {
	fixture := newOrdinarySubmissionReceiptFailureFixture(t)
	var err error
	fixture.permit, err = fixture.td.PrepareSubmission(fixture.supervisor.Binding())
	if err != nil {
		t.Fatal(err)
	}
	response := newResponse("dispatch")
	response.RootID, response.TaskID, response.TaskRecord = fixture.req.RootID, fixture.req.TaskID, "created"
	result := submissionCommandResult(response, tasklifecycle.SubmitWithClient(fixture.td, fixture.req, &fixture.meta, fixture.supervisor, fixture.runner, fixture.permit))
	if result.err != nil || result.code != 0 {
		t.Fatalf("initial staged submission = %+v", result)
	}
	receipt, err := fixture.td.ReadSupervisorReceipt()
	if err != nil {
		t.Fatal(err)
	}
	numericTaskID := receipt.NumericTaskID
	identity := pueue.Identity{
		RootID: receipt.RootID, TaskID: receipt.TaskID,
		SpecSHA256: receipt.SpecSHA256, MetaSHA256: receipt.MetaSHA256,
		NumericTaskID: &numericTaskID,
	}
	stale := pueue.Observation{
		Matched: true, State: pueue.StateStashed, Group: "default",
		Identity: identity, Binding: receipt.SupervisorRef(), NumericTaskID: numericTaskID,
	}
	writeRunnerStatus(t, fixture.statusPath, fixture.req.RootID, fixture.req.TaskID, numericTaskID)
	recovered, err := tasklifecycle.RecoverStagedSubmission(t.Context(), fixture.stateRoot, fixture.td, fixture.supervisor, stale)
	if err != nil || !recovered.Matched || recovered.State != pueue.StateRunning || recovered.NumericTaskID != numericTaskID {
		t.Fatalf("running staged recovery race = %+v, err %v", recovered, err)
	}
	assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "enqueue", 1)
}

func TestReconcileStatusRefusesQueueObservationWithoutDurableReceipt(t *testing.T) {
	fixture := newOrdinarySubmissionReceiptFailureFixture(t)
	permit, err := fixture.td.PrepareSubmission(fixture.supervisor.Binding())
	if err != nil {
		t.Fatal(err)
	}
	fixture.permit = permit
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	if err = permit.Release(); err != nil {
		t.Fatal(err)
	}
	fixture.permit = nil

	const numericTaskID int64 = 7
	writeRunnerStatus(t, fixture.statusPath, fixture.req.RootID, fixture.req.TaskID, numericTaskID)
	_, _, spec, metaHash, err := fixture.td.PreparedIdentity()
	if err != nil {
		t.Fatal(err)
	}
	response := newResponse("status")
	var observed tasklifecycle.Observation
	observation, reconcileErr := tasklifecycle.ReconcileStatusWithClient(
		t.Context(), fixture.stateRoot, fixture.td, fixture.req, &fixture.meta,
		spec, metaHash, fixture.supervisor, &observed,
	)
	applyTaskObservation(&response, observed, fixture.stateRoot)
	if !observation.Matched || observation.State != pueue.StateRunning {
		t.Fatalf("queue observation was not reproduced: %+v", observation)
	}
	if !errors.Is(reconcileErr, task.ErrEvidenceFault) || !errors.Is(reconcileErr, os.ErrNotExist) {
		t.Fatalf("queue-only admission error = %v, want missing durable evidence", reconcileErr)
	}
	if response.Admission != task.AdmissionUnknown.String() {
		t.Fatalf("queue-only admission = %q, want unknown", response.Admission)
	}
	if _, err = fixture.td.ReadSupervisorReceipt(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("queue observation created admission authority: %v", err)
	}
	assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "enqueue", 0)
}

type ordinarySubmissionReceiptFailureFixture struct {
	stateRoot  string
	store      *taskdir.Store
	supervisor *pueue.Client
	options    pueue.Options
	statusPath string
	commandLog string
	runner     string
	req        *task.TaskRecord
	meta       task.MetaRecord
	td         *taskdir.TaskDir
	permit     *taskdir.SubmissionPermit
}

func newOrdinarySubmissionReceiptFailureFixture(t *testing.T) *ordinarySubmissionReceiptFailureFixture {
	t.Helper()
	fixture := &ordinarySubmissionReceiptFailureFixture{stateRoot: testsupport.CanonicalTempDir(t)}
	store, err := taskdir.InitStore(fixture.stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	fixture.store = store
	t.Cleanup(func() {
		fixture.releaseSubmissionResources(func(releaseErr error) {
			t.Error(releaseErr)
		})
		if fixture.supervisor != nil {
			if closeErr := fixture.supervisor.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
	})

	fixture.supervisor, fixture.options, fixture.statusPath, fixture.commandLog = newOrdinarySubmissionReceiptSupervisor(t)
	fixture.runner = filepath.Join(testsupport.CanonicalTempDir(t), "runner")
	writeRunnerFixture(t, fixture.runner, "#!/bin/sh\nexit 0\n")
	workspace, err := filepath.EvalSymlinks(testsupport.CanonicalTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	brief := []byte("ordinary submission receipt recovery")
	taskID := strings.Repeat("7", 32)
	requested := task.TaskConfig{Permission: "read-only", Budget: "1m0s"}
	fixture.req = &task.TaskRecord{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: taskID,
		Provider: "fixture:test", Mode: "read-only", CanonicalCwd: workspace,
		RequestedConfig: requested, BudgetNanos: int64(time.Minute),
		BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief)),
	}
	profile := appTestExecutionProfile(fixture.req, "/usr/bin/true", nil)
	fixture.meta = tasklifecycle.NewMeta(*fixture.req, profile, fixture.supervisor.Binding(), fixture.runner, task.RunnerOwnershipCustom, "app-test")
	fixture.td, err = store.CreateTask(taskID, fixture.req, brief, &fixture.meta)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *ordinarySubmissionReceiptFailureFixture) releaseSubmissionResources(report func(error)) {
	if f.store != nil {
		f.store.SetFaultInjector(nil)
	}
	if f.permit != nil {
		if err := f.permit.Release(); err != nil {
			report(err)
		} else {
			f.permit = nil
		}
	}
	if f.td != nil {
		if err := f.td.Close(); err != nil {
			report(err)
		} else {
			f.td = nil
		}
	}
	if f.store != nil {
		if err := f.store.Close(); err != nil {
			report(err)
		} else {
			f.store = nil
		}
	}
}

func submitOrdinarySubmissionReceiptFailure(t *testing.T, fixture *ordinarySubmissionReceiptFailureFixture, receiptErr error) commandResult {
	t.Helper()
	fixture.store.SetFaultInjector(&ordinarySupervisorReceiptFault{err: receiptErr})
	var err error
	fixture.permit, err = fixture.td.PrepareSubmission(fixture.supervisor.Binding())
	if err != nil {
		t.Fatal(err)
	}
	response := newResponse("dispatch")
	response.RootID, response.TaskID, response.TaskRecord = fixture.req.RootID, fixture.req.TaskID, "created"
	result := submissionCommandResult(response, tasklifecycle.SubmitWithClient(fixture.td, fixture.req, &fixture.meta, fixture.supervisor, fixture.runner, fixture.permit))
	return markDispatchSubmissionFailure(result)
}

func assertOrdinarySubmissionReceiptSubmission(t *testing.T, fixture *ordinarySubmissionReceiptFailureFixture, result commandResult, receiptErr error, numericTaskID int64) {
	t.Helper()
	var err error
	if result.code != 1 || !errors.Is(result.err, receiptErr) {
		t.Fatalf("receipt publication failure was not operational: %+v", result)
	}
	if !fixture.permit.IsConsumed() {
		t.Fatal("successful supervisor add did not consume the submission permit")
	}
	if result.response.Admission != task.AdmissionAdmitted.String() || result.response.Supervisor == nil || result.response.Supervisor.NumericTaskID == nil || *result.response.Supervisor.NumericTaskID != numericTaskID {
		t.Fatalf("successful add was not preserved as admitted with its numeric receipt: %+v", result.response)
	}
	if _, err = fixture.td.ReadSubmission(); err != nil {
		t.Fatalf("submission intent was not retained: %v", err)
	}
	if _, err = fixture.td.ReadSupervisorReceipt(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed receipt publication unexpectedly became durable: %v", err)
	}
}

func assertOrdinarySubmissionReceiptResponse(t *testing.T, fixture *ordinarySubmissionReceiptFailureFixture, result commandResult) {
	t.Helper()
	var err error
	var firstOutput bytes.Buffer
	if err = writeJSONResponse(&firstOutput, "dispatch", &result.response, result.err); err != nil {
		t.Fatal(err)
	}
	var firstResponse Response
	if err = json.Unmarshal(firstOutput.Bytes(), &firstResponse); err != nil {
		t.Fatal(err)
	}
	if firstResponse.Admission != task.AdmissionAdmitted.String() || firstResponse.Failure == nil || firstResponse.Failure.Code != "task_operation_failed" || firstResponse.Failure.Stage != "record-submission-evidence" || firstResponse.Failure.NextAction != "check_status" {
		t.Fatalf("receipt failure lost its admitted operational recovery contract: %+v", firstResponse)
	}
	assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "add", 1)
}

func repairOrdinarySubmissionReceiptFailure(t *testing.T, fixture *ordinarySubmissionReceiptFailureFixture, numericTaskID int64) {
	t.Helper()
	fixture.releaseSubmissionResources(func(err error) {
		t.Fatal(err)
	})

	writeRunnerStashedStatus(t, fixture.statusPath, fixture.req.RootID, fixture.req.TaskID, numericTaskID)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"status", fixture.req.TaskID, "--root", fixture.stateRoot, "--json"}, &stdout, &stderr, Dependencies{SupervisorOptions: fixture.options})
	if code != 0 {
		t.Fatalf("status repair exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var statusResponse Response
	if err := json.Unmarshal(stdout.Bytes(), &statusResponse); err != nil {
		t.Fatal(err)
	}
	if statusResponse.Admission != task.AdmissionAdmitted.String() || statusResponse.Supervisor == nil || statusResponse.Supervisor.NumericTaskID == nil || *statusResponse.Supervisor.NumericTaskID != numericTaskID {
		t.Fatalf("status did not preserve the existing admission: %+v", statusResponse)
	}
	assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "add", 1)
	assertOrdinarySubmissionCommandCount(t, fixture.commandLog, "enqueue", 1)

	reopened, err := taskdir.OpenStore(fixture.stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	reopenedTask, err := reopened.OpenTask(fixture.req.TaskID)
	if err != nil {
		if closeErr := reopened.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	defer closeAppTestTask(t, reopened, reopenedTask)
	receipt, err := reopenedTask.ReadSupervisorReceipt()
	if err != nil {
		t.Fatal(err)
	}
	if receipt.NumericTaskID != numericTaskID {
		t.Fatalf("status repaired the wrong supervisor receipt: %+v", receipt)
	}
}

func newOrdinarySubmissionReceiptSupervisor(t *testing.T) (*pueue.Client, pueue.Options, string, string) {
	t.Helper()
	base := testsupport.CanonicalTempDir(t)
	executable := filepath.Join(base, "pueue")
	configPath := filepath.Join(base, "pueue.json")
	statusPath := filepath.Join(base, "status.json")
	commandLog := filepath.Join(base, "commands.log")
	writeRunnerFixture(t, executable, `#!/bin/sh
set -eu
command=
help=false
for arg in "$@"; do
  case "$arg" in
    --version|status|add|enqueue) command="$arg" ;;
    --help) help=true ;;
  esac
done
if [ "$help" = true ]; then
  command="${command}-help"
fi
printf '%s\n' "$command" >> "$FAKE_COMMAND_LOG"
case "$command" in
  --version) printf '%s\n' 'pueue 4.0.4' ;;
  status) cat "$FAKE_STATUS_PATH" ;;
  add-help) printf '%s\n' '--stashed --print-task-id' ;;
  enqueue-help) printf '%s\n' 'Usage: pueue enqueue <TASK_IDS>...' ;;
  add) printf '%s\n' '7' ;;
  enqueue) ;;
  *) exit 64 ;;
esac
`)
	config, err := json.Marshal(map[string]any{"shared": map[string]string{
		"pueue_directory":    filepath.Join(base, "data"),
		"runtime_directory":  filepath.Join(base, "runtime"),
		"unix_socket_path":   filepath.Join(base, "runtime", "pueue.sock"),
		"alias_file":         filepath.Join(base, "aliases"),
		"pid_path":           filepath.Join(base, "runtime", "pueue.pid"),
		"shared_secret_path": filepath.Join(base, "secret"),
		"daemon_cert":        filepath.Join(base, "cert"),
		"daemon_key":         filepath.Join(base, "key"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	writeAppTestFile(t, configPath, config)
	writeAppTestFile(t, statusPath, []byte(`{"tasks":{},"groups":{}}`))
	options := pueue.Options{
		Environment:        append(pueue.DefaultEnvironment(), "FAKE_COMMAND_LOG="+commandLog, "FAKE_STATUS_PATH="+statusPath),
		ObservationTimeout: pueue.DefaultObservationTimeout,
	}
	supervisor, err := pueue.Bind(t.Context(), executable, configPath, options)
	if err != nil {
		t.Fatal(err)
	}
	return supervisor, options, statusPath, commandLog
}

func assertOrdinarySubmissionCommandCount(t *testing.T, path, command string, want int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range strings.Split(string(data), "\n") {
		if entry == command {
			count++
		}
	}
	if count != want {
		t.Fatalf("supervisor command %q count=%d, want %d; log=%q", command, count, want, data)
	}
}

var (
	errInjectedPredicate = errors.New("predicate evaluation failed")
	errInjectedReceipt   = errors.New("receipt completion failed")
)

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

type failingInterpreter struct {
	err error
}

func (failingInterpreter) Reference() task.PredicateRef { return task.FixturePredicateRef() }

func (f failingInterpreter) Evaluate(predicate.Input, predicate.Evidence, io.Writer) (task.Interpretation, error) {
	return task.Interpretation{}, f.err
}

type receiptErrorObserver struct{}

func (receiptErrorObserver) Observe([]byte)  {}
func (receiptErrorObserver) Complete() error { return errInjectedReceipt }

type ordinarySupervisorReceiptFault struct {
	taskdir.BaseFaultInjector
	err error
}

func (f *ordinarySupervisorReceiptFault) OnLink(_, destination string) error {
	if filepath.Base(destination) == "supervisor.ref.json" {
		return f.err
	}
	return nil
}

var _ taskdir.FaultInjector = (*ordinarySupervisorReceiptFault)(nil)
