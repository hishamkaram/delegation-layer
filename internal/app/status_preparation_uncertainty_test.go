package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// These public status tests remove the published queued-runner repair
// receipt, so status preparation must rescan the queue before it observes the
// task. This fixture has no listening socket, so healthy preparation runs two
// saved-client readiness probes before the task observation. Its scheduling
// receipt is already current, so no group command runs. A stale runner receipt
// adds a repair scan as the third status command, before the task observation,
// and republishes the same receipt. requireRepairScanOrdinal proves that
// sequence for every fixture.
const preparationRepairScanStatus = 3

const (
	healthyStatusCommands     = "A status\nA status\nA status\n"
	staleRepairStatusCommands = "A status\nA status\nA status\nA status\n"
	// preparationStatusCommands ends at the repair scan: the task
	// observation never started.
	preparationStatusCommands = "A status\nA status\nA status\n"
)

// Pure preparation uncertainty observed no queue row, but the local durable
// records still own the interruption or timeout offer. Status keeps the
// in-flight repair scan pending beside that offer, exits zero, and does not
// invent supervisor or admission facts.
func TestStatusPreparationInFlightUncertaintyKeepsLocalContinuation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		stage      rebootLifetimeStage
		budgetStop bool
		wantStatus string
	}{
		{name: "closed lifetime", stage: rebootLifetimeClosedPreviousBoot, wantStatus: "interrupted"},
		{name: "budget stop", stage: rebootLifetimeClosedPreviousBoot, budgetStop: true, wantStatus: "timed_out"},
		{name: "budget closure", stage: rebootLifetimeClosedBudget, wantStatus: "timed_out"},
		{name: "no local offer", stage: rebootLifetimeNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: tc.stage, launchEnvironment: true})
			if tc.budgetStop {
				recordRebootStop(t, fixture, "budget", "budget")
			}
			control := requireRepairScanOrdinal(t, fixture)
			requireControlOffer(t, control, tc.wantStatus)
			receipt := staleRunnerRepairReceipt(t, fixture)
			before := snapshotRebootTaskFiles(t, fixture.td.Dir)
			resetRebootCommandLog(t, fixture)
			synctest.Test(t, func(t *testing.T) {
				gate := newSupervisorStatusGate(t, preparationRepairScanStatus)
				// Only new observer clients transfer pending cleanup. The daemon
				// creator retains its captured options and sole Wait ownership.
				fixture.options.WaitForPendingClose = false
				fixture.options.Observer = gate.observe

				code, response := runRebootStatusJSON(t, fixture)
				gate.requireStillHeld(t)
				requireReadOnlyPreparation(t, fixture, receipt, preparationStatusCommands)
				if code != 0 || !strings.Contains(response.Error, pueue.ErrInFlight.Error()) {
					t.Fatalf("status = code %d error=%q, want exit 0 retaining %q", code, response.Error, pueue.ErrInFlight)
				}
				requirePreparationPendingOnly(t, response)
				requirePreparationOffer(t, fixture, control, response, tc.wantStatus)
			})
			requireRebootTaskFilesUnchanged(t, before)
		})
	}
}

// A concrete preparation fault stays the authoritative result even when it is
// joined with ErrUnknown: status exits 1 and reports no offer, guidance, or
// pending command.
func TestStatusPreparationConcreteFaultSuppressesUncertaintyContinuation(t *testing.T) {
	t.Parallel()
	malformed := []byte("{")
	var decodeProbe struct{}
	decodeErr := json.Unmarshal(malformed, &decodeProbe)
	if decodeErr == nil {
		t.Fatal("fixture bytes are not malformed JSON")
	}
	for _, tc := range []struct {
		name         string
		inject       func(*testing.T, *rebootFixture, string)
		wantCommands string
		diagnostics  []string
	}{
		{
			name: "malformed repair scan",
			inject: func(t *testing.T, fixture *rebootFixture, _ string) {
				injectRepairScanPayload(t, fixture, malformed)
			},
			wantCommands: preparationStatusCommands,
			diagnostics:  []string{pueue.ErrUnknown.Error(), pueue.ErrStatusOperational.Error()},
		},
		{
			name: "malformed repair receipt",
			inject: func(t *testing.T, fixture *rebootFixture, receipt string) {
				writeRunnerRepairReceipt(t, fixture, receipt, malformed)
			},
			wantCommands: "A status\nA status\n",
			diagnostics:  []string{decodeErr.Error()},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRebootFixture(t, rebootSetup{prior: testPriorSession(), identity: true, stage: rebootLifetimeClosedPreviousBoot, launchEnvironment: true})
			requireControlOffer(t, requireRepairScanOrdinal(t, fixture), "interrupted")
			receipt := staleRunnerRepairReceipt(t, fixture)
			tc.inject(t, fixture, receipt)
			before := snapshotRebootTaskFiles(t, fixture.td.Dir)
			resetRebootCommandLog(t, fixture)

			code, response := runRebootStatusJSON(t, fixture)
			requireFaultedStatusResponse(t, code, response, tc.diagnostics...)
			if response.Pending != nil || response.Supervisor != nil {
				t.Fatalf("faulted status pending=%+v supervisor=%+v, want neither", response.Pending, response.Supervisor)
			}
			if commands := readRebootCommandLog(t, fixture); commands != tc.wantCommands {
				t.Fatalf("faulted preparation commands = %q, want %q", commands, tc.wantCommands)
			}
			requireRebootTaskFilesUnchanged(t, before)
		})
	}
}

// requireRepairScanOrdinal is the control fixture for the repair-scan status
// ordinal. It returns the unheld stale-receipt status response.
func requireRepairScanOrdinal(t *testing.T, fixture *rebootFixture) Response {
	t.Helper()
	resetRebootCommandLog(t, fixture)
	if code, _ := runRebootStatusJSON(t, fixture); code != 0 {
		t.Fatalf("healthy control status exit = %d, want 0", code)
	}
	if commands := readRebootCommandLog(t, fixture); commands != healthyStatusCommands {
		t.Fatalf("healthy control commands = %q, want %q", commands, healthyStatusCommands)
	}
	receipt := staleRunnerRepairReceipt(t, fixture)
	resetRebootCommandLog(t, fixture)
	code, response := runRebootStatusJSON(t, fixture)
	if code != 0 || response.Error != "" || response.Pending != nil {
		t.Fatalf("stale-receipt control status = code %d %+v, want a clean exit 0", code, response)
	}
	if commands := readRebootCommandLog(t, fixture); commands != staleRepairStatusCommands {
		t.Fatalf("stale-receipt control commands = %q, want %q", commands, staleRepairStatusCommands)
	}
	if names := runnerRepairReceipts(t, fixture); len(names) != 1 || names[0] != receipt {
		t.Fatalf("repair control receipts = %q, want republished %q", names, receipt)
	}
	return response
}

func requireControlOffer(t *testing.T, control Response, wantStatus string) {
	t.Helper()
	if control.Status != wantStatus || (control.Continuation != nil) != (wantStatus != "") {
		t.Fatalf("control status = %q continuation=%+v, want %q", control.Status, control.Continuation, wantStatus)
	}
	if wantStatus != "" && !control.Continuation.Resumable {
		t.Fatalf("control continuation = %+v, want resumable", control.Continuation)
	}
}

// requirePreparationPendingOnly proves the uncertain response reports the
// in-flight command and no queue-derived supervisor or admission fact.
func requirePreparationPendingOnly(t *testing.T, response Response) {
	t.Helper()
	if response.Pending == nil || response.Pending.Kind != string(tasklifecycle.PendingSupervisor) {
		t.Fatalf("status pending = %+v, want %s", response.Pending, tasklifecycle.PendingSupervisor)
	}
	if response.Supervisor != nil || response.Admission != task.AdmissionUnknown.String() || response.Liveness != task.LivenessUndetermined.String() {
		t.Fatalf("status invented queue facts: supervisor=%+v admission=%q liveness=%q", response.Supervisor, response.Admission, response.Liveness)
	}
	if response.Outcome != nil || response.Payload != nil || response.Publication != task.PublicationUnknown.String() {
		t.Fatalf("status invented publication: outcome=%+v payload=%+v publication=%q", response.Outcome, response.Payload, response.Publication)
	}
}

// requirePreparationOffer checks that the uncertain response carries exactly
// the continuation and guidance the healthy control derived from local records.
func requirePreparationOffer(t *testing.T, fixture *rebootFixture, control, response Response, wantStatus string) {
	t.Helper()
	if response.Status != wantStatus {
		t.Fatalf("status = %q, want %q", response.Status, wantStatus)
	}
	switch wantStatus {
	case "":
		if response.Continuation != nil || response.Failure != nil {
			t.Fatalf("status without local evidence offered continuation=%+v failure=%+v", response.Continuation, response.Failure)
		}
		return
	case "interrupted":
		failure := response.Failure
		if failure == nil || failure.Code != "task_interrupted" || failure.Stage != "status" || failure.NextAction != "continue_task" {
			t.Fatalf("status failure = %+v, want task_interrupted/continue_task", failure)
		}
	default:
		if response.Failure != nil {
			t.Fatalf("%s status failure = %+v, want none", wantStatus, response.Failure)
		}
	}
	requireInterruptedContinuation(t, fixture, response, "observed", true)
	if *response.Continuation != *control.Continuation {
		t.Fatalf("uncertain continuation = %+v, want control %+v", response.Continuation, control.Continuation)
	}
}

// requireReadOnlyPreparation proves the held repair scan was the last command,
// no queue mutation ran, and no repair receipt was published.
func requireReadOnlyPreparation(t *testing.T, fixture *rebootFixture, receipt, wantCommands string) {
	t.Helper()
	if commands := readRebootCommandLog(t, fixture); commands != wantCommands {
		t.Fatalf("preparation commands = %q, want %q", commands, wantCommands)
	}
	for _, name := range runnerRepairReceipts(t, fixture) {
		if name == receipt {
			t.Fatalf("status published repair receipt %s without a completed scan", receipt)
		}
	}
}

// repairScanPayload replaces the queue status payload at the entry of the
// repair-scan `status --json`, before the real command starts.
type repairScanPayload struct {
	path    string
	payload []byte

	mu      sync.Mutex
	entries int
	err     error
}

func (f *repairScanPayload) observe(event pueue.CommandEvent) {
	if event.Stage != "entry" || !historicalCommandSequence(event.Argv, "status", "--json") {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries++
	if f.entries == preparationRepairScanStatus {
		f.err = os.WriteFile(f.path, f.payload, 0o600)
	}
}

func injectRepairScanPayload(t *testing.T, fixture *rebootFixture, payload []byte) {
	t.Helper()
	healthy, err := os.ReadFile(fixture.statusPath)
	if err != nil {
		t.Fatal(err)
	}
	fault := &repairScanPayload{path: fixture.statusPath, payload: payload}
	fixture.options.Observer = fault.observe
	t.Cleanup(func() {
		fault.mu.Lock()
		defer fault.mu.Unlock()
		if fault.err != nil || fault.entries != preparationRepairScanStatus {
			t.Errorf("repair-scan fault entries=%d err=%v, want injection at status %d", fault.entries, fault.err, preparationRepairScanStatus)
		}
		if restoreErr := os.WriteFile(fixture.statusPath, healthy, 0o600); restoreErr != nil {
			t.Error(restoreErr)
		}
	})
}

func runnerRepairDir(fixture *rebootFixture) string {
	return filepath.Join(fixture.root, "runner-repair")
}

func runnerRepairReceipts(t *testing.T, fixture *rebootFixture) []string {
	t.Helper()
	entries, err := os.ReadDir(runnerRepairDir(fixture))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "r-") && strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	return names
}

// staleRunnerRepairReceipt removes the sole published repair receipt and
// returns its name.
func staleRunnerRepairReceipt(t *testing.T, fixture *rebootFixture) string {
	t.Helper()
	names := runnerRepairReceipts(t, fixture)
	if len(names) != 1 {
		t.Fatalf("repair receipts = %q, want exactly one", names)
	}
	if err := os.Remove(filepath.Join(runnerRepairDir(fixture), names[0])); err != nil {
		t.Fatal(err)
	}
	return names[0]
}

func writeRunnerRepairReceipt(t *testing.T, fixture *rebootFixture, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(runnerRepairDir(fixture), name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func resetRebootCommandLog(t *testing.T, fixture *rebootFixture) {
	t.Helper()
	if err := os.WriteFile(fixture.commandLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readRebootCommandLog(t *testing.T, fixture *rebootFixture) string {
	t.Helper()
	data, err := os.ReadFile(fixture.commandLog)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
