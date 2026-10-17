package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPreparedMigrationRefusesAdmissionAndCleanupProbes(t *testing.T) {
	for _, probe := range []struct {
		name string
		run  func(context.Context, string, string, string, Options) (*Client, bool, error)
	}{
		{name: "cleanup", run: ProbePrivate},
		{name: "admission-groups", run: func(ctx context.Context, client, daemon, root string, options Options) (*Client, bool, error) {
			return ProbePrivateGroupsWithGate(ctx, client, daemon, root, options, nil)
		}},
	} {
		t.Run(probe.name, func(t *testing.T) {
			fixture := newBusyPreparedPrivateObservation(t)
			binding := fixture.legacy.Binding()
			before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
			client, ready, err := probe.run(t.Context(), binding.ClientExecutable, binding.DaemonExecutable, fixture.stateRoot, fixture.legacy.options)
			if client != nil {
				if closeErr := client.CloseAfterPending(err); closeErr != nil {
					t.Error(closeErr)
				}
			}
			if err != nil || ready {
				t.Errorf("prepared migration granted %s authority: ready=%t err=%v", probe.name, ready, err)
			}
			assertPreparedPrivateObservationUnchanged(t, fixture, before)
		})
	}
}

func TestPreparedMigrationRefusesAdmissionAtLiveEndpoint(t *testing.T) {
	fixture := newBusyPreparedPrivateObservation(t)
	binding := fixture.legacy.Binding()
	listener := listenPrivateTestEndpoint(t, binding.Endpoint)
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	})
	client, ready, err := ProbePrivateEndpointWithGate(t.Context(), binding.ClientExecutable, binding.DaemonExecutable, fixture.stateRoot, fixture.legacy.options, nil)
	if client != nil {
		if closeErr := client.CloseAfterPending(err); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if err != nil || ready {
		t.Errorf("live endpoint granted admission during prepared migration: ready=%t err=%v", ready, err)
	}
	assertBusyPreparedPrivateObservation(t, fixture)
}

func TestPreparedMigrationRefusesCompletedTaskRemovalFromHeldClient(t *testing.T) {
	fixture := newBusyPreparedPrivateObservation(t)
	identity := writeCompletedPrivateRemovalTask(t, fixture.stateRoot)
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	err := fixture.legacy.RemoveCompletedTask(t.Context(), identity)
	if !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Errorf("held client removed a completed task during prepared migration: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-kill-task-id")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("prepared migration permitted a remove command: %v", statErr)
	}
	assertPreparedPrivateObservationUnchanged(t, fixture, before)
}

func writeCompletedPrivateRemovalTask(t *testing.T, root string) Identity {
	t.Helper()
	rootID, taskID := preparedPrivateRootID(t, root), strings.Repeat("b", 32)
	numericTaskID := int64(42)
	status := statusTestPayload(t, map[string]any{
		"42": statusTestJob(numericTaskID, "delegate:"+rootID+":"+taskID, map[string]any{"Done": map[string]any{
			"enqueued_at": statusTestTime, "start": statusTestTime, "end": statusTestTime, "result": "Success",
		}}),
	}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	base := filepath.Join(root, privateSupervisorDirectory)
	writePrivateSuccessorTestBytes(t, filepath.Join(base, "test-status.json"), status)
	writePrivateSuccessorTestBytes(t, filepath.Join(base, "test-record-kill"), nil)
	return Identity{
		RootID: rootID, TaskID: taskID, NumericTaskID: &numericTaskID,
		SpecSHA256: strings.Repeat("c", 64), MetaSHA256: strings.Repeat("d", 64),
	}
}

func TestPreparedMigrationRefusesCompletedTaskRemovalThroughRecoveryAlias(t *testing.T) {
	fixture := newBusyPreparedPrivateObservation(t)
	binding := fixture.legacy.Binding()
	client, _, found, err := recoverPrivateRecoveryBinding(t.Context(), fixture.stateRoot, binding, binding.ClientExecutable, binding.DaemonExecutable, fixture.legacy.options)
	if err != nil || !found || client == nil {
		t.Fatalf("recover prepared alias client: found=%t client=%v err=%v", found, client, err)
	}
	t.Cleanup(func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if client.RuntimeBinding().ConfigPath != privateRecoveryConfigPath(fixture.stateRoot) {
		t.Fatal("fixture did not exercise the retained recovery alias")
	}
	identity := writeCompletedPrivateRemovalTask(t, fixture.stateRoot)
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	if err = client.RemoveCompletedTask(t.Context(), identity); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Errorf("alias cleanup did not defer incomplete migration: %v", err)
	}
	assertPreparedPrivateObservationUnchanged(t, fixture, before)
}

func TestCompletedSuccessorAllowsCleanupThroughRecoveredPair(t *testing.T) {
	for _, compatible := range []bool{false, true} {
		t.Run(map[bool]string{false: "published-pair", true: "compatible-pair"}[compatible], func(t *testing.T) {
			fixture := newCompletedPrivateSuccessorTestFixture(t, true)
			clientPath, daemonPath := fixture.clientPath, fixture.daemonPath
			if compatible {
				clientPath, daemonPath = installPrivateSupervisorPair(t, filepath.Join(fixture.base, "compatible"))
				removePrivateTestPath(t, filepath.Join(fixture.base, "completed"))
			}
			client, err := RecoverPrivate(t.Context(), fixture.stateRoot, fixture.saved, clientPath, daemonPath, fixture.options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if closeErr := client.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			})
			identity := writeCompletedPrivateRemovalTask(t, fixture.stateRoot)
			if err = client.RemoveCompletedTask(t.Context(), identity); err != nil {
				t.Fatalf("completed successor cleanup through recovered pair: %v", err)
			}
			path := filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-kill-task-id")
			if got := strings.TrimSpace(string(readPrivateSupervisorFile(t, path))); got != "42" {
				t.Errorf("cleanup removed task %q, want 42", got)
			}
		})
	}
}

func TestPreparedMigrationRefusesQueuedRunnerMutationFromHeldClient(t *testing.T) {
	fixture := newBusyPreparedPrivateObservation(t)
	rootID, taskID := preparedPrivateRootID(t, fixture.stateRoot), strings.Repeat("b", 32)
	replacement := RunnerCommandReplacement{
		NumericID: 42, RootID: rootID, TaskID: taskID, Label: "delegate:" + rootID + ":" + taskID,
		RootPath: fixture.stateRoot, OldRunner: filepath.Join(fixture.stateRoot, "old-runner"),
		NewRunner: fixture.legacy.Binding().ClientExecutable,
	}
	job := statusTestJob(42, replacement.Label, map[string]any{"Queued": map[string]any{"enqueued_at": statusTestTime}})
	job["original_command"], job["command"] = runnerCommand(replacement.OldRunner, replacement, false), runnerCommand(replacement.OldRunner, replacement, false)
	status := statusTestPayload(t, map[string]any{"42": job}, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	writePrivateSuccessorTestBytes(t, filepath.Join(fixture.stateRoot, privateSupervisorDirectory, "test-status.json"), status)
	var edited atomic.Bool
	fixture.legacy.options.Observer = func(event CommandEvent) {
		if slices.Contains(event.Argv, "edit") {
			edited.Store(true)
		}
	}
	before := snapshotPrivateSuccessorTestState(t, fixture.stateRoot)
	if err := fixture.legacy.ReplaceQueuedRunnerCommands(t.Context(), []RunnerCommandReplacement{replacement}); !errors.Is(err, ErrPrivateSupervisorMigrationRequired) {
		t.Errorf("prepared migration permitted runner repair: %v", err)
	}
	if edited.Load() {
		t.Error("prepared migration reached a queued-command edit")
	}
	assertPreparedPrivateObservationUnchanged(t, fixture, before)
}

func preparedPrivateRootID(t *testing.T, root string) string {
	t.Helper()
	var record task.RootRecord
	if err := task.DecodeStrict(readPrivateSupervisorFile(t, filepath.Join(root, "root.json")), &record); err != nil {
		t.Fatal(err)
	}
	if err := task.ValidateRootRecord(&record); err != nil {
		t.Fatal(err)
	}
	return record.RootID
}
