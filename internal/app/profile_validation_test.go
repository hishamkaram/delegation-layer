package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestDispatchExistingRefreshesProfileBeforeSubmission(t *testing.T) {
	store, td, req := newAppTestTask(t, false)
	defer closeAppTestTask(t, store, td)

	var profileCalls int
	deps := Dependencies{PrepareProfile: func(task.TaskRecord) (PreparedProfile, error) {
		profileCalls++
		profile := appTestExecutionProfile(req, "/tmp/app-provider", nil)
		profile.Effective.Digest = task.ComputeSHA256([]byte("changed-policy"))
		return profile, nil
	}}
	result := dispatchExisting(Arguments{}, deps, store, td, *req, newResponse("dispatch"))
	if result.code != 2 || !errors.Is(result.err, task.ErrIdentityMismatch) {
		t.Fatalf("changed profile was admitted: %+v", result)
	}
	if profileCalls != 1 {
		t.Fatalf("profile was refreshed %d times before retry submission", profileCalls)
	}
	if _, err := td.ReadSubmission(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("profile refusal created admission authority: %v", err)
	}
}

func TestRunRunnerRefreshesProfileAfterSupervisorReconcile(t *testing.T) {
	fixture := newRunnerRefreshFixture(t)
	defer fixture.close(t)

	var profileCalls int
	providerStarted := false
	deps := Dependencies{
		PrepareProfile: func(task.TaskRecord) (PreparedProfile, error) {
			profileCalls++
			profile := fixture.profile()
			if profileCalls == 2 {
				if _, err := os.Stat(filepath.Join(fixture.td.Dir, "provider.start")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("final profile refresh ran after start intent was prepared: %v", err)
				}
				profile.Effective.Digest = task.ComputeSHA256([]byte("changed-policy"))
			}
			return profile, nil
		},
		SupervisorOptions: pueue.Options{Environment: fixture.environment},
		ExecutionHooks: execution.Hooks{Start: func(*exec.Cmd) error {
			providerStarted = true
			return errors.New("provider launch reached test guard")
		}},
	}
	result := runRunner(runnerArguments{Root: fixture.store.Root, TaskID: fixture.req.TaskID}, deps)
	if result.code != 2 || !errors.Is(result.err, task.ErrIdentityMismatch) {
		t.Fatalf("changed profile crossed prelaunch boundary: %+v", result)
	}
	if profileCalls != 2 {
		t.Fatalf("profile refresh count=%d, want initial and final checks", profileCalls)
	}
	if providerStarted {
		t.Fatal("provider start hook ran after final profile mismatch")
	}
	for _, name := range []string{"provider.start", "provider.started.json", "provider.exit", "outcome.json"} {
		if _, err := os.Stat(filepath.Join(fixture.td.Dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("runner created %s after profile mismatch: %v", name, err)
		}
	}
}

func TestRunRunnerRefreshesProfileAtProcessStartBoundary(t *testing.T) {
	fixture := newRunnerRefreshFixture(t)
	defer fixture.close(t)

	var profileCalls int
	providerStarted := false
	deps := Dependencies{
		PrepareProfile: func(task.TaskRecord) (PreparedProfile, error) {
			profileCalls++
			profile := fixture.profile()
			if profileCalls == 3 {
				if _, err := os.Stat(filepath.Join(fixture.td.Dir, "provider.start")); err != nil {
					t.Fatalf("start-boundary profile refresh ran before start intent was prepared: %v", err)
				}
				profile.Effective.Digest = task.ComputeSHA256([]byte("changed-policy"))
			}
			return profile, nil
		},
		SupervisorOptions: pueue.Options{Environment: fixture.environment},
		ExecutionHooks: execution.Hooks{Start: func(*exec.Cmd) error {
			providerStarted = true
			return errors.New("provider launch reached test guard")
		}},
	}
	result := runRunner(runnerArguments{Root: fixture.store.Root, TaskID: fixture.req.TaskID}, deps)
	if result.code != 0 || result.err != nil || result.response.Outcome == nil || result.response.Publication != task.PublicationRejected.String() {
		t.Fatalf("changed profile was not sealed as a start refusal: %+v", result)
	}
	if profileCalls != 3 {
		t.Fatalf("profile refresh count=%d, want pre-admission, reconciliation, and start-boundary checks", profileCalls)
	}
	if providerStarted {
		t.Fatal("provider start hook ran after final profile mismatch")
	}
	inspection, err := fixture.td.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if !inspection.StartExists || !inspection.SealExists || !inspection.OutcomeExists || inspection.StartedExists {
		t.Fatalf("start-boundary refusal left incomplete evidence: %+v", inspection)
	}
}

type runnerRefreshFixture struct {
	base        string
	store       *taskdir.Store
	td          *taskdir.TaskDir
	req         *task.TaskRecord
	supervisor  *pueue.Client
	environment []string
}

func newRunnerRefreshFixture(t *testing.T) *runnerRefreshFixture {
	t.Helper()
	rawBase, err := os.MkdirTemp("/tmp", "dl-runner-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cleanupErr := os.RemoveAll(rawBase); cleanupErr != nil {
			t.Error(cleanupErr)
		}
	})
	base, err := filepath.EvalSymlinks(rawBase)
	if err != nil {
		t.Fatal(err)
	}
	supervisorExecutable := filepath.Join(base, "pueue")
	configPath := filepath.Join(base, "pueue.json")
	statusPath := filepath.Join(base, "status.json")
	writeRunnerSupervisor(t, supervisorExecutable, configPath, statusPath, base)
	environment := append(os.Environ(), "RUNNER_STATUS_PATH="+statusPath)
	supervisor, err := pueue.Bind(context.Background(), supervisorExecutable, configPath, pueue.Options{Environment: environment})
	if err != nil {
		t.Fatal(err)
	}
	store, err := taskdir.InitStore(filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(base, "workspace")
	if err = os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	brief := []byte("brief")
	id := strings.Repeat("8", 32)
	requested := task.TaskConfig{Permission: "read-only", Budget: "1m0s"}
	req := &task.TaskRecord{SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: id, Provider: "fixture:test", Mode: "read-only", CanonicalCwd: cwd, RequestedConfig: requested, BudgetNanos: int64(time.Minute), BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief))}
	digest := task.ComputeSHA256([]byte("runner-profile"))
	providerExecutable, err := filepath.EvalSymlinks("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	meta := &task.MetaRecord{SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: id, RequestedConfig: requested, EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: digest}, Containment: "fixture-only", Approval: "never", ProviderExecutable: providerExecutable, ProviderVersion: "fixture-v2", PublisherBuild: "app-test", PublisherVersion: "app-test", Predicate: task.FixturePredicateRef(), SupervisorConfig: supervisor.Binding(), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	td, err := store.CreateTask(id, req, brief, meta)
	if err != nil {
		t.Fatal(err)
	}
	permit, err := td.PrepareSubmission(supervisor.Binding())
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.Release(); err != nil {
		t.Fatal(err)
	}
	writeRunnerStatus(t, statusPath, store.RootID, id, 7)
	return &runnerRefreshFixture{base: base, store: store, td: td, req: req, supervisor: supervisor, environment: environment}
}

func (f *runnerRefreshFixture) profile() PreparedProfile {
	providerExecutable, err := filepath.EvalSymlinks("/usr/bin/true")
	if err != nil {
		providerExecutable = "/usr/bin/true"
	}
	providerBytes, err := os.ReadFile(providerExecutable)
	if err != nil {
		providerBytes = nil
	}
	return PreparedProfile{
		Plan:            execution.Plan{Executable: providerExecutable, ExecutableSHA256: task.ComputeSHA256(providerBytes), Directory: f.req.CanonicalCwd, Predicate: task.FixturePredicateRef()},
		ObservedVersion: "fixture-v2",
		Effective:       task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: task.ComputeSHA256([]byte("runner-profile"))},
	}
}

func (f *runnerRefreshFixture) close(t *testing.T) {
	t.Helper()
	if err := f.td.Close(); err != nil {
		t.Error(err)
	}
	if err := f.store.Close(); err != nil {
		t.Error(err)
	}
}

func writeRunnerSupervisor(t *testing.T, executable, configPath, statusPath, base string) {
	t.Helper()
	script := "#!/bin/sh\nset -eu\ncase \"${3-}\" in\n  --version) printf '%s\\n' 'pueue 4.0.4' ;;\n  status) cat \"$RUNNER_STATUS_PATH\" ;;\n  *) exit 64 ;;\nesac\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	shared := map[string]any{
		"pueue_directory":    filepath.Join(base, "data"),
		"runtime_directory":  filepath.Join(base, "run"),
		"unix_socket_path":   filepath.Join(base, "socket"),
		"alias_file":         filepath.Join(base, "aliases"),
		"pid_path":           filepath.Join(base, "pid"),
		"shared_secret_path": filepath.Join(base, "secret"),
		"daemon_cert":        filepath.Join(base, "cert"),
		"daemon_key":         filepath.Join(base, "key"),
	}
	data, err := json.Marshal(map[string]any{"shared": shared})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(statusPath, []byte(`{"tasks":{},"groups":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeRunnerStatus(t *testing.T, path, rootID, taskID string, numericID int64) {
	t.Helper()
	const statusTime = "2026-09-14T00:00:00.123456789Z"
	label := fmt.Sprintf("delegate:%s:%s", rootID, taskID)
	job := map[string]any{
		"id": numericID, "created_at": statusTime, "original_command": "runner", "command": "runner", "path": "/tmp", "envs": map[string]string{}, "group": "default", "dependencies": []int64{}, "priority": int32(0), "label": label,
		"status": map[string]any{"Running": map[string]any{"enqueued_at": statusTime, "start": statusTime}},
	}
	data, err := json.Marshal(map[string]any{"tasks": map[string]any{"7": job}, "groups": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
