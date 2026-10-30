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

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

const savedPrivateCancelStatusTime = "2026-09-14T00:00:00.123456789Z"

// savedPrivateCancelFixture composes a ready saved private pair, a production
// task store, and one admitted queue row. The pair is a shell fixture, not a
// native Pueue daemon; it proves the public command composition only.
type savedPrivateCancelFixture struct {
	root, client, daemon, runner         string
	statusPath, commandLog, daemonStarts string
	readyPath                            string
	options                              pueue.Options
	saved                                task.SupervisorRef
	bound                                *pueue.Client
	store                                *taskdir.Store
	td                                   *taskdir.TaskDir
	taskID                               string
}

type savedPrivateCancelStatus struct {
	Tasks  map[string]savedPrivateCancelJob   `json:"tasks"`
	Groups map[string]savedPrivateCancelGroup `json:"groups"`
}

type savedPrivateCancelGroup struct {
	Status        string `json:"status"`
	ParallelTasks int    `json:"parallel_tasks"`
}

type savedPrivateCancelJob struct {
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
	Status          savedPrivateCancelVariant `json:"status"`
}

type savedPrivateCancelVariant struct {
	Queued *savedPrivateCancelQueued `json:"Queued,omitempty"`
	Done   *savedPrivateCancelDone   `json:"Done,omitempty"`
}

type savedPrivateCancelQueued struct {
	EnqueuedAt string `json:"enqueued_at"`
}

type savedPrivateCancelDone struct {
	EnqueuedAt string `json:"enqueued_at"`
	Start      string `json:"start"`
	End        string `json:"end"`
	Result     string `json:"result"`
}

func newSavedPrivateCancelFixture(t *testing.T, state pueue.State) *savedPrivateCancelFixture {
	t.Helper()
	base := testsupport.CanonicalTempDir(t)
	fixture := &savedPrivateCancelFixture{
		root:         filepath.Join(base, "state"),
		client:       filepath.Join(base, "pair", "pueue"),
		daemon:       filepath.Join(base, "pair", "pueued"),
		runner:       filepath.Join(base, "runner"),
		statusPath:   filepath.Join(base, "status.json"),
		commandLog:   filepath.Join(base, "commands.log"),
		daemonStarts: filepath.Join(base, "daemon-starts.log"),
		readyPath:    filepath.Join(base, "private-ready"),
		options:      newHistoricalSupervisorOptions(),
	}
	workspace := filepath.Join(base, "workspace")
	for _, directory := range []string{filepath.Dir(fixture.client), workspace} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pidPath := filepath.Join(fixture.root, ".supervisor", "run", "pueue.pid")
	writeRunnerFixture(t, fixture.client, historicalSupervisorClientScript("A", fixture.readyPath, pidPath, fixture.statusPath, fixture.commandLog))
	writeRunnerFixture(t, fixture.daemon, historicalSupervisorDaemonScript("A", fixture.readyPath, pidPath, fixture.daemonStarts))
	writeRunnerFixture(t, fixture.runner, "#!/bin/sh\nexit 0\n")
	store, err := taskdir.InitStore(fixture.root)
	if err != nil {
		t.Fatal(err)
	}
	fixture.store = store
	t.Cleanup(func() { fixture.close(t) })
	fixture.bound, err = pueue.BindPrivate(t.Context(), fixture.client, fixture.daemon, fixture.root, fixture.options)
	if err != nil {
		t.Fatalf("bind saved private pair: %v", err)
	}
	fixture.saved = fixture.bound.Binding()
	if fixture.saved.DaemonExecutable == "" || fixture.saved.DaemonSHA256 == "" {
		t.Fatalf("saved private pair lacks daemon provenance: %+v", fixture.saved)
	}
	fixture.admitTask(t, workspace, state)
	if err = os.WriteFile(fixture.commandLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *savedPrivateCancelFixture) admitTask(t *testing.T, workspace string, state pueue.State) {
	t.Helper()
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	brief := []byte("saved private cancel")
	request := newHistoricalSupervisorTaskRecord(f.store.RootID, canonicalWorkspace, brief)
	profile := appTestExecutionProfile(&request, f.runner, nil)
	meta := tasklifecycle.NewMeta(request, profile, f.saved, f.runner, task.RunnerOwnershipCustom, "saved-private-cancel-test")
	if f.td, err = f.store.CreateTask(request.TaskID, &request, brief, &meta); err != nil {
		t.Fatal(err)
	}
	f.taskID = request.TaskID
	permit, err := f.td.PrepareSubmission(f.saved)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := historicalConsumeSubmissionPermit(permit)
	if err != nil {
		t.Fatal(err)
	}
	numericTaskID := int64(7)
	identity := pueue.Identity{RootID: submission.RootID, TaskID: submission.TaskID, SpecSHA256: submission.SpecSHA256, MetaSHA256: submission.MetaSHA256, NumericTaskID: &numericTaskID}
	writeSavedPrivateCancelStatus(t, f.statusPath, identity.Label(), state)
	observation := pueue.Observation{Matched: true, State: state, Identity: identity, Binding: f.saved, NumericTaskID: numericTaskID}
	receipt, err := observation.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	if err = f.td.RecordSupervisorReceipt(*receipt); err != nil {
		t.Fatal(err)
	}
}

func writeSavedPrivateCancelStatus(t *testing.T, path, label string, state pueue.State) {
	t.Helper()
	var status savedPrivateCancelVariant
	switch state {
	case pueue.StateQueued:
		status.Queued = &savedPrivateCancelQueued{EnqueuedAt: savedPrivateCancelStatusTime}
	case pueue.StateEnded:
		status.Done = &savedPrivateCancelDone{EnqueuedAt: savedPrivateCancelStatusTime, Start: savedPrivateCancelStatusTime, End: savedPrivateCancelStatusTime, Result: "Success"}
	case pueue.StateUnknown, pueue.StateStashed, pueue.StateRunning:
		t.Fatalf("unsupported saved private cancel state %q", state)
	}
	data, err := json.Marshal(savedPrivateCancelStatus{
		Tasks: map[string]savedPrivateCancelJob{"7": {
			ID: 7, CreatedAt: savedPrivateCancelStatusTime, OriginalCommand: "runner", Command: "runner", Path: filepath.Dir(path),
			Envs: map[string]string{}, Group: "default", Dependencies: []int64{}, Label: label, Status: status,
		}},
		Groups: map[string]savedPrivateCancelGroup{"default": {Status: "Running"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *savedPrivateCancelFixture) close(t *testing.T) {
	t.Helper()
	if f.bound != nil {
		if _, err := os.Stat(f.readyPath); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), pueue.PrivateControlTimeout(f.options))
			if shutdownErr := f.bound.Shutdown(ctx); shutdownErr != nil {
				t.Error(shutdownErr)
			} else if waitErr := f.bound.WaitUnavailable(ctx); waitErr != nil {
				t.Error(waitErr)
			}
			cancel()
		}
		if err := f.bound.Close(); err != nil {
			t.Error(err)
		}
	}
	for _, closer := range []func() error{f.tdClose, f.storeClose} {
		if err := closer(); err != nil {
			t.Error(err)
		}
	}
}

func (f *savedPrivateCancelFixture) tdClose() error {
	if f.td == nil {
		return nil
	}
	return f.td.Close()
}

func (f *savedPrivateCancelFixture) storeClose() error {
	if f.store == nil {
		return nil
	}
	return f.store.Close()
}

func (f *savedPrivateCancelFixture) cancel(t *testing.T) (int, Response) {
	t.Helper()
	deps := Dependencies{
		ResourceChecker:             fixtureResources,
		PredicateRegistry:           predicate.Default,
		SupervisorOptions:           f.options,
		InitialSupervisorExecutable: f.client,
		RunnerExecutable:            f.runner,
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"cancel", f.taskID, "--root", f.root, "--json"}, &stdout, &stderr, deps)
	return code, decodeHistoricalResponse(t, stdout.Bytes(), stderr.Bytes(), code)
}

func (f *savedPrivateCancelFixture) commands(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.commandLog)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// snapshot captures the immutable task history and private supervisor state
// that a saved-binding cancel may read but must never rewrite.
func (f *savedPrivateCancelFixture) snapshot(t *testing.T) []map[string][]byte {
	t.Helper()
	starts, err := os.ReadFile(f.daemonStarts)
	if err != nil {
		t.Fatal(err)
	}
	return []map[string][]byte{
		snapshotRebootTaskFiles(t, f.td.Dir),
		snapshotRebootTaskFiles(t, filepath.Join(f.root, ".supervisor")),
		{f.daemonStarts: starts},
	}
}

func (f *savedPrivateCancelFixture) assertHistoryRetained(t *testing.T, before []map[string][]byte, response Response) {
	t.Helper()
	for _, files := range before {
		requireRebootTaskFilesUnchanged(t, files)
	}
	if _, err := os.Lstat(filepath.Join(f.root, ".supervisor", "migration.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("saved private cancel created a migration journal: %v", err)
	}
	records, err := f.td.ReadStopRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Request == nil || response.Stop == nil || records[0].Request.RequestID != response.Stop.RequestID {
		t.Fatalf("cancel did not persist exactly its own stop request: records=%d response=%+v", len(records), response.Stop)
	}
	if records[0].Request.Supervisor != f.saved {
		t.Fatalf("stop request supervisor was not the immutable saved binding:\n got=%+v\nwant=%+v", records[0].Request.Supervisor, f.saved)
	}
}

func TestPublicCancelRemovesQueuedRowThroughReadySavedPrivatePair(t *testing.T) {
	fixture := newSavedPrivateCancelFixture(t, pueue.StateQueued)
	before := fixture.snapshot(t)
	code, response := fixture.cancel(t)
	stop := response.Stop
	if code != 0 || stop == nil || !stop.Requested || !stop.Matched || !stop.Attempted || stop.Action != "remove" || stop.NumericTaskID == nil || *stop.NumericTaskID != 7 || stop.Acknowledged == nil || !*stop.Acknowledged {
		t.Fatalf("public cancel did not remove the queued saved private row: exit=%d stop=%+v error=%+v", code, stop, response.Error)
	}
	commands := fixture.commands(t)
	if got := strings.Count(commands, "A remove\n"); got != 1 || strings.Contains(commands, "A kill\n") || strings.Contains(commands, "A add\n") {
		t.Fatalf("public cancel queue commands = %q, want exactly one remove", commands)
	}
	fixture.assertHistoryRetained(t, before, response)
}

func TestPublicCancelObservesEndedRowThroughReadySavedPrivatePair(t *testing.T) {
	fixture := newSavedPrivateCancelFixture(t, pueue.StateEnded)
	before := fixture.snapshot(t)
	code, response := fixture.cancel(t)
	stop := response.Stop
	if code != 0 || stop == nil || !stop.Requested || !stop.Matched || stop.Attempted || stop.ObservedState != string(pueue.StateEnded) || stop.NumericTaskID == nil || *stop.NumericTaskID != 7 {
		t.Fatalf("public cancel did not observe the ended saved private row: exit=%d stop=%+v error=%+v", code, stop, response.Error)
	}
	commands := fixture.commands(t)
	if !strings.Contains(commands, "A status\n") || strings.Contains(commands, "A kill\n") || strings.Contains(commands, "A remove\n") || strings.Contains(commands, "A add\n") {
		t.Fatalf("ended-row cancel queue commands = %q, want observation without mutation", commands)
	}
	fixture.assertHistoryRetained(t, before, response)
}
