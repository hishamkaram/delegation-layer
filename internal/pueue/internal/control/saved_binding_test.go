package control

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// savedBindingStatusPayload is the typed Pueue status wire fixture for one
// labelled default-group row.
type savedBindingStatusPayload struct {
	Tasks  map[string]savedBindingStatusJob   `json:"tasks"`
	Groups map[string]savedBindingStatusGroup `json:"groups"`
}

type savedBindingStatusGroup struct {
	Status        string `json:"status"`
	ParallelTasks int    `json:"parallel_tasks"`
}

type savedBindingStatusJob struct {
	ID              int64                     `json:"id"`
	CreatedAt       string                    `json:"created_at"`
	OriginalCommand string                    `json:"original_command"`
	Command         string                    `json:"command"`
	Path            string                    `json:"path"`
	Envs            map[string]string         `json:"envs"`
	Group           string                    `json:"group"`
	Dependencies    []int64                   `json:"dependencies"`
	Priority        int32                     `json:"priority"`
	Label           string                    `json:"label"`
	Status          savedBindingStatusVariant `json:"status"`
}

type savedBindingStatusVariant struct {
	Queued *savedBindingQueuedState `json:"Queued,omitempty"`
	Done   *savedBindingDoneState   `json:"Done,omitempty"`
}

type savedBindingQueuedState struct {
	EnqueuedAt string `json:"enqueued_at"`
}

type savedBindingDoneState struct {
	EnqueuedAt string `json:"enqueued_at"`
	Start      string `json:"start"`
	End        string `json:"end"`
	Result     string `json:"result"`
}

// savedPrivateBindingFixture is one live saved private pair with a task whose
// immutable meta, submission, and receipt carry the full private binding.
type savedPrivateBindingFixture struct {
	private    privateSupervisorTestFixture
	saved      task.SupervisorRef
	task       *taskFixture
	submission *task.SubmitRecord
	statusPath string
	killPath   string
	controlLog string
	options    Options
}

func newSavedPrivateBindingFixture(t *testing.T, state State) *savedPrivateBindingFixture {
	t.Helper()
	private := newPrivateSupervisorTestFixture(t)
	bound := bindPrivateForTest(t, private.stateRoot, private.clientPath, private.daemonPath)
	t.Cleanup(func() {
		if err := bound.Close(); err != nil {
			t.Error(err)
		}
	})
	privateRoot := filepath.Join(private.stateRoot, privateSupervisorDirectory)
	fixture := &savedPrivateBindingFixture{
		private:    private,
		saved:      bound.Binding(),
		statusPath: filepath.Join(privateRoot, "test-status.json"),
		killPath:   filepath.Join(privateRoot, "test-kill-task-id"),
		controlLog: filepath.Join(privateRoot, "test-control-arguments"),
	}
	fixture.options = Options{
		ObservationTimeout: privateTestObservationTimeout,
		Environment:        append(os.Environ(), "PRIVATE_CONTROL_MARKER="+fixture.controlLog),
	}
	if fixture.saved.DaemonExecutable == "" || fixture.saved.DaemonSHA256 == "" {
		t.Fatalf("private fixture binding lacks daemon provenance: %+v", fixture.saved)
	}
	if err := os.WriteFile(filepath.Join(privateRoot, "test-record-kill"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.task = newTaskFixture(t, fixture.saved)
	fixture.submission = createSubmission(t, fixture.task, fixture.saved)
	writeSavedBindingStatus(t, fixture.statusPath, fixture.submission, 7, state)
	observation, err := bound.Reconcile(t.Context(), identityFromRecord(fixture.submission))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := observation.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.task.taskDir.RecordSupervisorReceipt(*receipt); err != nil {
		t.Fatal(err)
	}
	removePrivateTestPath(t, fixture.controlLog)
	return fixture
}

func writeSavedBindingStatus(t *testing.T, path string, submission *task.SubmitRecord, id int64, state State) {
	t.Helper()
	var status savedBindingStatusVariant
	switch state {
	case StateQueued:
		status.Queued = &savedBindingQueuedState{EnqueuedAt: statusTestTime}
	case StateEnded:
		status.Done = &savedBindingDoneState{EnqueuedAt: statusTestTime, Start: statusTestTime, End: statusTestTime, Result: "Success"}
	case StateUnknown, StateStashed, StateRunning:
		t.Fatalf("unsupported saved-binding status fixture state %q", state)
	}
	job := savedBindingStatusJob{
		ID: id, CreatedAt: statusTestTime, OriginalCommand: "runner", Command: "runner", Path: filepath.Dir(path),
		Envs: map[string]string{}, Group: "default", Dependencies: []int64{},
		Label: identityFromRecord(submission).Label(), Status: status,
	}
	data, err := json.Marshal(savedBindingStatusPayload{
		Tasks:  map[string]savedBindingStatusJob{"7": job},
		Groups: map[string]savedBindingStatusGroup{"default": {Status: "Running"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *savedPrivateBindingFixture) bindSaved(t *testing.T) *Client {
	t.Helper()
	client, err := BindSaved(t.Context(), f.saved, f.options)
	if err != nil {
		t.Fatalf("BindSaved rejected the unchanged saved private pair: %v", errors.Join(err, client.CloseAfterPending(err)))
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return client
}

// queueMutations counts each queue-mutating command the client fixture saw.
func (f *savedPrivateBindingFixture) queueMutations(t *testing.T) map[string]int {
	t.Helper()
	data, err := os.ReadFile(f.controlLog)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]int{}
	}
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]int{}
	for _, argument := range strings.Fields(string(data)) {
		switch argument {
		case "kill", "remove", "add", "enqueue":
			mutations[argument]++
		}
	}
	return mutations
}

func (f *savedPrivateBindingFixture) assertNoQueueMutation(t *testing.T) {
	t.Helper()
	assertPrivateTestPathMissing(t, f.killPath)
	if mutations := f.queueMutations(t); len(mutations) != 0 {
		t.Fatalf("refused saved binding sent queue mutations: %v", mutations)
	}
}

func TestBindSavedAuthenticatesRecordedPrivateDaemon(t *testing.T) {
	fixture := newSavedPrivateBindingFixture(t, StateQueued)
	client := fixture.bindSaved(t)
	if client.Binding() != fixture.saved {
		t.Fatalf("saved private binding lost recorded provenance:\n got=%+v\nwant=%+v", client.Binding(), fixture.saved)
	}
	if client.RuntimeBinding() != fixture.saved {
		t.Fatalf("saved private runtime binding differs from the authenticated pair:\n got=%+v\nwant=%+v", client.RuntimeBinding(), fixture.saved)
	}
	assertPrivateDaemonStartCount(t, fixture.private.stateRoot, 1)
}

func TestBindSavedPrivateStopRemovesQueuedRowOnce(t *testing.T) {
	fixture := newSavedPrivateBindingFixture(t, StateQueued)
	client := fixture.bindSaved(t)
	permit := createStopPermit(t, fixture.task)
	result, err := client.Stop(t.Context(), permit)
	if releaseErr := permit.Release(); releaseErr != nil {
		t.Fatal(releaseErr)
	}
	if err != nil {
		t.Fatalf("saved private stop failed: result=%+v err=%v", result, err)
	}
	if !result.Requested || !result.Matched || !result.Attempted || result.Action != "remove" || result.ObservedState != StateQueued || result.NumericTaskID == nil || *result.NumericTaskID != 7 || result.Acknowledged == nil || !*result.Acknowledged {
		t.Fatalf("saved private stop did not remove the queued row: %+v", result)
	}
	if got := string(readPrivateSupervisorFile(t, fixture.killPath)); got != "7\n" {
		t.Fatalf("saved private stop targeted %q, want exactly task 7", got)
	}
	if mutations := fixture.queueMutations(t); len(mutations) != 1 || mutations["remove"] != 1 {
		t.Fatalf("queued stop sent queue mutations %v, want exactly one remove", mutations)
	}
	assertPrivateDaemonStartCount(t, fixture.private.stateRoot, 1)
}

func TestBindSavedPrivateStopObservesEndedRowWithoutMutation(t *testing.T) {
	fixture := newSavedPrivateBindingFixture(t, StateEnded)
	client := fixture.bindSaved(t)
	permit := createStopPermit(t, fixture.task)
	result, err := client.Stop(t.Context(), permit)
	if releaseErr := permit.Release(); releaseErr != nil {
		t.Fatal(releaseErr)
	}
	if err != nil {
		t.Fatalf("saved private stop of an ended row failed: result=%+v err=%v", result, err)
	}
	if !result.Requested || !result.Matched || result.Attempted || result.ObservedState != StateEnded || result.NumericTaskID == nil || *result.NumericTaskID != 7 {
		t.Fatalf("saved private stop did not observe the ended row: %+v", result)
	}
	fixture.assertNoQueueMutation(t)
}

func TestBindSavedPrivateRefusesMissingRecordedDaemon(t *testing.T) {
	fixture := newSavedPrivateBindingFixture(t, StateQueued)
	removePrivateTestPath(t, fixture.private.daemonPath)
	client, err := BindSaved(t.Context(), fixture.saved, fixture.options)
	if client == nil {
		t.Fatal("BindSaved discarded its bound client after daemon authentication failed")
	}
	if closeErr := client.CloseAfterPending(err); closeErr != nil {
		t.Fatal(closeErr)
	}
	if !errors.Is(err, ErrBinding) {
		t.Fatalf("missing recorded private daemon error = %v, want ErrBinding", err)
	}
	fixture.assertNoQueueMutation(t)
	assertPrivateDaemonStartCount(t, fixture.private.stateRoot, 1)
}

// Changed installation provenance remains an allowed observation; every
// strict mutation gate still refuses it before any queue command.
func TestBindSavedPrivateChangedProvenanceObservesButRefusesMutations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   func(*savedPrivateBindingFixture) string
		differ func(got, saved task.SupervisorRef) bool
	}{
		{
			name: "modified daemon",
			path: func(f *savedPrivateBindingFixture) string { return f.private.daemonPath },
			differ: func(got, saved task.SupervisorRef) bool {
				return got.DaemonExecutable == saved.DaemonExecutable && got.DaemonSHA256 != saved.DaemonSHA256 && got.ClientSHA256 == saved.ClientSHA256
			},
		},
		{
			name: "modified client",
			path: func(f *savedPrivateBindingFixture) string { return f.private.clientPath },
			differ: func(got, saved task.SupervisorRef) bool {
				return got.ClientSHA256 != saved.ClientSHA256 && got.DaemonSHA256 == saved.DaemonSHA256
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newSavedPrivateBindingFixture(t, StateQueued)
			appendPrivateSupervisorFile(t, tc.path(fixture), "\n# changed after the saved binding\n")
			client := fixture.bindSaved(t)
			if !tc.differ(client.Binding(), fixture.saved) || !task.SameSupervisorIdentity(client.Binding(), fixture.saved) {
				t.Fatalf("changed provenance was not authenticated from current bytes:\n got=%+v\nsaved=%+v", client.Binding(), fixture.saved)
			}
			observation, err := client.Reconcile(t.Context(), identityFromRecord(fixture.submission))
			if err != nil || !observation.Matched || observation.State != StateQueued {
				t.Fatalf("changed provenance lost its read-only observation: %+v %v", observation, err)
			}
			assertSavedBindingMutationsRefused(t, fixture, client)
		})
	}
}

func assertSavedBindingMutationsRefused(t *testing.T, fixture *savedPrivateBindingFixture, client *Client) {
	t.Helper()
	permit := createStopPermit(t, fixture.task)
	result, err := client.Stop(t.Context(), permit)
	if releaseErr := permit.Release(); releaseErr != nil {
		t.Fatal(releaseErr)
	}
	if !errors.Is(err, ErrBinding) || !result.Requested || result.Matched || result.Attempted || result.NumericTaskID != nil {
		t.Fatalf("changed provenance stop = %+v %v, want ErrBinding before numeric assignment", result, err)
	}
	for name, submit := range map[string]func(*taskFixture) error{
		"submit": func(target *taskFixture) error {
			permit, prepareErr := target.taskDir.PrepareSubmission(fixture.saved)
			if prepareErr != nil {
				return prepareErr
			}
			_, submitErr := client.Submit(t.Context(), permit, Launch{})
			return errors.Join(submitErr, permit.Release())
		},
		"staged submit": func(target *taskFixture) error {
			permit, prepareErr := target.taskDir.PrepareSubmission(fixture.saved)
			if prepareErr != nil {
				return prepareErr
			}
			_, submitErr := client.SubmitStaged(t.Context(), permit, Launch{}, func(Observation) error {
				t.Error("refused staged submission reached its commit")
				return nil
			})
			return errors.Join(submitErr, permit.Release())
		},
	} {
		if submitErr := submit(newTaskFixture(t, fixture.saved)); !errors.Is(submitErr, ErrBinding) {
			t.Fatalf("changed provenance %s error = %v, want ErrBinding", name, submitErr)
		}
	}
	fixture.assertNoQueueMutation(t)
	assertPrivateDaemonStartCount(t, fixture.private.stateRoot, 1)
}

func TestBindSavedLegacyPrivateWithoutDaemonKeepsClientOnlyBinding(t *testing.T) {
	fixture := newSavedPrivateBindingFixture(t, StateQueued)
	legacy := fixture.saved
	legacy.DaemonExecutable, legacy.DaemonSHA256 = "", ""
	client, err := BindSaved(t.Context(), legacy, fixture.options)
	if err != nil {
		t.Fatalf("legacy private saved binding was rejected: %v", errors.Join(err, client.CloseAfterPending(err)))
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if client.Binding() != legacy || client.RuntimeBinding() != legacy {
		t.Fatalf("legacy private saved binding changed:\n got=%+v\nwant=%+v", client.Binding(), legacy)
	}
}

func TestBindSavedExternalRefIgnoresUnavailableRecordedDaemon(t *testing.T) {
	fake := newFakeSupervisor(t, "normal")
	saved := fake.client.Binding()
	saved.DaemonExecutable = filepath.Join(filepath.Dir(fake.configPath), "missing-pueued")
	saved.DaemonSHA256 = task.ComputeSHA256([]byte("unavailable external daemon"))
	client, err := BindSaved(t.Context(), saved, Options{ObservationTimeout: DefaultObservationTimeout, Environment: fake.environment})
	if err != nil {
		t.Fatalf("external saved binding required its recorded daemon: %v", errors.Join(err, client.CloseAfterPending(err)))
	}
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	if got := client.Binding(); got != fake.client.Binding() || got.DaemonExecutable != "" || got.DaemonSHA256 != "" {
		t.Fatalf("external saved observation changed:\n got=%+v\nwant=%+v", got, fake.client.Binding())
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if readyErr := client.Ready(ctx); readyErr != nil {
		t.Fatalf("external saved binding lost its read-only observation: %v", readyErr)
	}
}

func TestBindSavedPrivatePendingVersionProbeRetainsOwnerWithoutAttach(t *testing.T) {
	fixture := newSavedPrivateBindingFixture(t, StateQueued)
	writeExecutable(t, fixture.private.clientPath, bindingProbePendingTestScript+privateClientFixture)
	base := filepath.Join(fixture.private.stateRoot, privateSupervisorDirectory)
	gate := newStopIdentityLeaseTestGate(t, base, "saved-private-version")
	options := fixture.options
	options.Environment = append(append([]string{}, fixture.options.Environment...), "BINDING_PROBE_GATE="+filepath.Join(base, "saved-private-version"))
	options.ObservationTimeout = 10 * time.Second
	owner, probeErr := detachBindingProbePendingTest(t, gate, func(ctx context.Context) (*Client, error) {
		return BindSaved(ctx, fixture.saved, options)
	})
	if owner == nil {
		t.Fatal("saved private BindSaved discarded the owner of its pending version probe")
	}
	pending := requireBindingProbePendingTestHandle(t, probeErr, "--version")
	if errors.Is(probeErr, ErrBinding) || owner.Binding().DaemonExecutable != "" || owner.Binding().DaemonSHA256 != "" {
		t.Fatalf("pending version probe was classified or attached: binding=%+v err=%v", owner.Binding(), probeErr)
	}
	completeBindingProbePendingTest(t, gate, pending)
	fixture.assertNoQueueMutation(t)
}
