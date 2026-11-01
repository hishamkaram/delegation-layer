package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type schedulerRunnerRequest struct {
	name   string
	global bool
	json   bool
}

func schedulerRunnerRequests() []schedulerRunnerRequest {
	return []schedulerRunnerRequest{
		{name: "global/human", global: true},
		{name: "global/json", global: true, json: true},
		{name: "postcommand/human"},
		{name: "postcommand/json", json: true},
	}
}

func (r schedulerRunnerRequest) arguments(root, runner string) []string {
	args := []string{"--root", root}
	if r.global {
		args = append(args, "--runner", runner, "scheduler", "run")
	} else {
		args = append(args, "scheduler", "run", "--runner", runner)
	}
	if r.json {
		args = append(args, "--json")
	}
	return args
}

func TestSchedulerCustomRunnerRejectsBeforeRootInitialization(t *testing.T) {
	for _, request := range schedulerRunnerRequests() {
		t.Run(request.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "absent")
			runner := filepath.Join(t.TempDir(), "custom-delegate-run")
			writeRunnerFixture(t, runner, "custom runner")
			var stdout, stderr bytes.Buffer
			code := RunContext(schedulerStartupContext(t), request.arguments(root, runner), &stdout, &stderr, schedulerRunnerRejectionDependencies(t))
			requireSchedulerRunnerRejection(t, request.json, code, &stdout, &stderr)
			if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("custom runner rejection created the missing root or inspection failed: %v", err)
			}
		})
	}
}

func TestSchedulerCustomRunnerRejectsBeforeExistingOwnerLease(t *testing.T) {
	for _, request := range schedulerRunnerRequests() {
		t.Run(request.name, func(t *testing.T) {
			root, store, definition := scheduleFixture(t)
			first, skipEnd, active := seedScheduleHistory(t, store, definition)
			holdSchedulerRunnerOwnerLease(t, root)
			runner := filepath.Join(t.TempDir(), "custom-delegate-run")
			writeRunnerFixture(t, runner, "custom runner")
			var stdout, stderr bytes.Buffer
			code := RunContext(schedulerStartupContext(t), request.arguments(root, runner), &stdout, &stderr, schedulerRunnerRejectionDependencies(t))
			requireSchedulerRunnerRejection(t, request.json, code, &stdout, &stderr)
			requireSchedulerRunnerStoreUnchanged(t, store, definition, first, skipEnd, active)
		})
	}
}

func requireSchedulerRunnerRejection(t *testing.T, jsonOutput bool, code int, stdout, stderr *bytes.Buffer) {
	t.Helper()
	if code != 2 {
		t.Errorf("scheduler custom runner exit=%d, want 2; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	message := stderr.String()
	if jsonOutput {
		var response Response
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
			t.Fatalf("decode scheduler response %q: %v; stderr=%q", stdout.String(), err, stderr.String())
		}
		if response.SchemaVersion != OutputSchemaVersion || response.Command != "scheduler" || stderr.Len() != 0 {
			t.Errorf("scheduler response=%+v stderr=%q", response, stderr.String())
		}
		message = response.Error
	}
	requireSchedulerRunnerGuidance(t, message)
}

func requireSchedulerRunnerGuidance(t *testing.T, message string) {
	t.Helper()
	for _, guidance := range []string{"managed worker", "omit --runner", "matching colocated", "delegate/delegate-run"} {
		if !strings.Contains(message, guidance) {
			t.Errorf("scheduler custom runner error %q lacks recovery guidance %q", message, guidance)
		}
	}
}

// The store owns cursor/history/reservations; rejected startup has no mutation
// authority. Read through its domain API, independent of SQLite file layout.
func requireSchedulerRunnerStoreUnchanged(t *testing.T, store contract.Store, definition contract.Definition, first contract.Occurrence, skipEnd time.Time, active contract.Occurrence) {
	t.Helper()
	stored, err := store.GetSchedule(t.Context(), definition.ScheduleID)
	if err != nil || stored.State != contract.StateEnabled || !stored.Cursor.Equal(active.Due) {
		t.Errorf("custom runner rejection changed the schedule cursor/state: %+v err=%v", stored, err)
	}
	wantHistory := contract.HistoryPageResult{Entries: []contract.HistoryEntry{
		contract.OccurrenceEntry(active),
		contract.SkipEntry(contract.SkipSpan{From: first.Due, To: skipEnd, Reason: contract.SkipMissedUnavailable}),
		contract.OccurrenceEntry(first),
	}}
	history, err := store.History(t.Context(), contract.HistoryPage{ScheduleID: definition.ScheduleID, Limit: contract.MaxPageLimit})
	if err != nil || !reflect.DeepEqual(history, wantHistory) {
		t.Errorf("custom runner rejection changed history: got=%+v want=%+v err=%v", history, wantHistory, err)
	}
	wantActive := contract.ActivePageResult{Occurrences: []contract.Occurrence{active}}
	reservations, err := store.ActiveOccurrences(t.Context(), contract.ActivePage{Limit: contract.MaxPageLimit})
	if err != nil || !reflect.DeepEqual(reservations, wantActive) {
		t.Errorf("custom runner rejection changed reservations: got=%+v want=%+v err=%v", reservations, wantActive, err)
	}
	for _, want := range []contract.Occurrence{first, active} {
		occurrence, readErr := store.GetOccurrence(t.Context(), want.TaskID)
		if readErr != nil || !reflect.DeepEqual(occurrence, want) {
			t.Errorf("custom runner rejection changed occurrence %s: got=%+v want=%+v err=%v", want.TaskID, occurrence, want, readErr)
		}
	}
}

func holdSchedulerRunnerOwnerLease(t *testing.T, root string) {
	t.Helper()
	state, err := taskdir.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := state.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	lease, err := scheduler.AcquireOwnerLease(t.Context(), state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
}

func schedulerRunnerRejectionDependencies(t *testing.T) Dependencies {
	t.Helper()
	deps := schedulerStartupDependencies(t)
	var storeCalls atomic.Int64
	deps.PredicateRegistry = func() predicate.Registry {
		storeCalls.Add(1)
		return NativePredicates()
	}
	t.Cleanup(func() {
		if calls := storeCalls.Load(); calls != 0 {
			t.Errorf("custom runner rejection reached task-store initialization/open: calls=%d", calls)
		}
	})
	return deps
}

func TestSchedulerRecordedCustomRunnerWithoutPathRejectsBeforeRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	args := Arguments{Root: root, runnerOwnership: task.RunnerOwnershipCustom}
	var stderr bytes.Buffer
	result := runScheduler(schedulerStartupContext(t), args, schedulerRunnerRejectionDependencies(t), &stderr)
	if result.code != 2 || result.err == nil {
		t.Fatalf("recorded custom ownership without a path: code=%d err=%v", result.code, result.err)
	}
	requireSchedulerRunnerGuidance(t, result.err.Error())
	if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("recorded custom runner rejection created the missing root or inspection failed: %v", err)
	}
}

func TestSchedulerAutomaticInjectedRunnerReachesOwnerLease(t *testing.T) {
	root, _, _ := scheduleFixture(t)
	holdSchedulerRunnerOwnerLease(t, root)
	deps := schedulerStartupDependencies(t)
	deps.RunnerExecutable = filepath.Join(t.TempDir(), "delegate-run")
	writeRunnerFixture(t, deps.RunnerExecutable, "automatic runner")
	var stdout, stderr bytes.Buffer
	code := RunContext(schedulerStartupContext(t), []string{"scheduler", "run", "--root", root, "--json"}, &stdout, &stderr, deps)
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode scheduler response %q: %v; stderr=%q", stdout.String(), err, stderr.String())
	}
	if code != 1 || !strings.Contains(response.Error, task.ErrLockBusy.Error()) {
		t.Fatalf("automatic runner did not reach owner lease: code=%d response=%+v stderr=%q", code, response, stderr.String())
	}
}
