package probe

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const probePostStartCanary = "private-probe-post-start-fault"

type probePostStartLifetime struct {
	command    *exec.Cmd
	starts     atomic.Int32
	waits      atomic.Int32
	finishes   atomic.Int32
	closeFault error
}

func (l *probePostStartLifetime) start(command *exec.Cmd, start func() error) error {
	l.command = command
	if err := start(); err != nil {
		return err
	}
	l.starts.Add(1)
	return nil
}

func (l *probePostStartLifetime) StartCommand(command *exec.Cmd, start func() error) error {
	if err := l.start(command, start); err != nil {
		return err
	}
	return errors.New(probePostStartCanary)
}

func (l *probePostStartLifetime) StartProjectCommand(command *exec.Cmd, directory *os.File, _ task.ProjectLaunchContext, start func() error) error {
	if err := l.start(command, start); err != nil {
		return err
	}
	// Inject the existing owner's deferred selection.Close fault after real
	// native Start, using the same technique as execution/post_start_fault_test.
	l.closeFault = directory.Close()
	return l.closeFault
}

func (l *probePostStartLifetime) FinishCommand(_ context.Context, command *exec.Cmd) error {
	l.finishes.Add(1)
	if command != l.command || command.ProcessState == nil || !command.ProcessState.Exited() {
		return errors.New("FinishCommand did not follow the owned native Wait")
	}
	return nil
}

func (*probePostStartLifetime) Terminate(context.Context) error { return nil }
func (*probePostStartLifetime) CloseTask(context.Context) error { return nil }

func TestProbePostStartFaultRetainsWaitAndFinish(t *testing.T) {
	for _, route := range []string{"runtime", "models"} {
		for _, project := range []bool{false, true} {
			name := map[bool]string{false: "lifetime_control", true: "project_selection_close"}[project]
			t.Run(route+"/"+name, func(t *testing.T) {
				assertProbePostStartFault(t, route, project)
			})
		}
	}
}

func assertProbePostStartFault(t *testing.T, route string, project bool) {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shell := locateTestShell(t)
	lifetime := &probePostStartLifetime{}
	options := execution.SupervisedOptions{Stopper: runtimeTestStopper{}, Lifetime: lifetime}
	if project {
		options.Project = probePostStartPermit(t, directory, shell.Path)
	}
	// The old probe skips Wait. The child is finite and exits by itself; reap
	// it even on the expected red, before test-owned directories are removed.
	t.Cleanup(func() {
		command := lifetime.command
		if command != nil && command.Process != nil && command.ProcessState == nil {
			if waitErr := command.Wait(); waitErr != nil {
				t.Errorf("join child abandoned by probe: %v", waitErr)
			}
		}
	})
	returnedFacts := false
	var probeErr error
	workErr, stopErr := execution.RunSupervised(time.Now().Add(time.Minute), options, func(scope execution.PreflightScope) error {
		returnedFacts, probeErr = runPostStartProbe(route, scope, directory, shell.Path, lifetime)
		return probeErr
	})
	assertPostStartChildOwnership(t, route, lifetime)
	if lifetime.closeFault != nil || probeErr == nil || workErr == nil || stopErr != nil || returnedFacts {
		t.Fatalf("post-Start fault was hidden or produced facts: close=%v probe=%v work=%v stop=%v facts=%v", lifetime.closeFault, probeErr, workErr, stopErr, returnedFacts)
	}
	if strings.Contains(probeErr.Error(), probePostStartCanary) {
		t.Fatalf("private fault escaped inspection redaction: %v", probeErr)
	}
}

func assertPostStartChildOwnership(t *testing.T, route string, lifetime *probePostStartLifetime) {
	t.Helper()
	if lifetime.starts.Load() != 1 || lifetime.finishes.Load() != 1 {
		var state *os.ProcessState
		if lifetime.command != nil {
			state = lifetime.command.ProcessState
		}
		t.Fatalf("post-Start fault lost child ownership: Start=%d FinishCommand=%d ProcessState=%v", lifetime.starts.Load(), lifetime.finishes.Load(), state)
	}
	if route == "models" && lifetime.waits.Load() != 1 {
		t.Fatalf("model helper did not receive its sole Wait: %d", lifetime.waits.Load())
	}
	if lifetime.command.ProcessState == nil || !lifetime.command.ProcessState.Success() {
		t.Fatalf("finite native helper was not successfully joined: %v", lifetime.command.ProcessState)
	}
}

func runPostStartProbe(route string, scope execution.PreflightScope, directory, executable string, lifetime *probePostStartLifetime) (bool, error) {
	arguments := []string{"-c", "printf '%s\\n' '--fixture'; printf '%s\\n' '" + probePostStartCanary + "' >&2"}
	if route == "runtime" {
		facts, err := InspectRuntime(scope, provider.RuntimeProbeDefinition{
			Executable: executable, Directory: directory, Environment: []string{}, HelpArgs: arguments, RequiredFlags: []string{"--fixture"},
		}, 128)
		return facts != (provider.RuntimeFacts{}), err
	}
	result, err := runModelCommand(scope, provider.InspectionDefinition{
		Executable: executable, Directory: directory, Environment: []string{}, OutputLimit: 128,
	}, arguments, nil, nativeHooks{wait: func(command *exec.Cmd) error {
		lifetime.waits.Add(1)
		return command.Wait()
	}})
	return len(result.stdout) != 0 || len(result.stderr) != 0 || result.success, err
}

func probePostStartPermit(t *testing.T, directory, executable string) *taskdir.StartPermit {
	t.Helper()
	// This private state root is outside the helper's selected working directory.
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "native"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = store.EnableWorkspaceConnectorContext(t.Context(), true, true); err != nil {
		t.Fatal(err)
	}
	root, err := store.RootTokenContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	project, err := taskdir.ObservePathProjectContext(t.Context(), directory)
	if err != nil {
		t.Fatal(err)
	}
	brief := []byte("finite preflight helper")
	config := task.TaskConfig{Permission: "read-only", Budget: "1m0s"}
	request := &task.TaskRecord{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: strings.Repeat("a", 32), Provider: "fixture:test", Mode: "read-only",
		CanonicalCwd: directory, RequestedConfig: config, BudgetNanos: int64(time.Minute), BriefSHA256: task.ComputeSHA256(brief), BriefLength: int64(len(brief)),
		ConnectorContext: &task.ConnectorContext{RootToken: root, Project: project},
	}
	meta := &task.MetaRecord{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, TaskID: request.TaskID, RequestedConfig: config,
		EffectiveConfig: task.EffectiveConfig{Containment: "fixture-only", Approval: "never", Digest: task.ComputeSHA256([]byte("probe fixture"))},
		Containment:     "fixture-only", Approval: "never", ProviderExecutable: executable, ProviderVersion: "fixture-v1", PublisherBuild: "test", PublisherVersion: "test",
		Predicate: task.FixturePredicateRef(), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		SupervisorConfig: task.SupervisorRef{ConfigPath: "/fake/pueue.yml", ConfigDigest: task.ComputeSHA256([]byte("pueue fixture")), Endpoint: "/fake/socket", ObservedVersion: "fixture-v1"},
	}
	prepared, err := store.CreateTask(request.TaskID, request, brief, meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := prepared.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	permit, err := prepared.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := permit.Release(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	return permit
}
