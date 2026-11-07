package control

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestManagedRepairNativePreservesQueuedAndStashedIdentity(t *testing.T) {
	owner := os.Getenv("DELEGATE_REPAIR_EDITOR_OWNER")
	if owner == "" {
		t.Skip("actual built delegate-run is required")
	}
	for _, state := range []State{StateQueued, StateStashed} {
		t.Run(string(state), func(t *testing.T) { testManagedRepairNativeState(t, owner, state) })
	}
}

func testManagedRepairNativeState(t *testing.T, owner string, state State) {
	t.Helper()
	fixture := newNativePrivateSupervisorFixture(t)
	// The generic native fixture only needs a root record. Repair admission
	// instead exercises a fully initialized production store and its locks.
	fixture.stateRoot = filepath.Join(filepath.Dir(fixture.stateRoot), "repair-state")
	store, err := taskdir.InitStoreWithPredicatesContext(t.Context(), fixture.stateRoot, predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			fixture.lifetime.retained = true
			t.Error(closeErr)
		}
	})
	client, err := fixture.bind(t)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.command(t.Context(), nil, "pause", "--wait"); err != nil {
		t.Fatal(err)
	}
	plan := prepareManagedRepairNativeRow(t, client, store, owner, state)
	t.Cleanup(func() { client.options.ControlOwnerExecutable = "" })
	client.options.ControlOwnerExecutable = owner
	ctx := t.Context()
	if err = client.ReplaceQueuedRunnerCommands(ctx, []RunnerCommandReplacement{plan}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	job, found := queueJob(snapshot, plan.NumericID)
	if !found || len(snapshot.Jobs) != 1 || job.State != state || job.Label == nil || *job.Label != plan.Label || originalCommand(job) != runnerCommand(plan.NewRunner, plan, true) {
		t.Fatalf("repair changed row identity/state: found=%t job=%+v", found, job)
	}
}

func prepareManagedRepairNativeRow(t *testing.T, client *Client, store *taskdir.Store, owner string, state State) RunnerCommandReplacement {
	t.Helper()
	plan := RunnerCommandReplacement{RootID: store.RootID, TaskID: strings.Repeat("b", 32), RootPath: store.Root, OldRunner: client.runtimeBinding.ClientExecutable, NewRunner: owner}
	plan.Label = "delegate:" + plan.RootID + ":" + plan.TaskID
	args := []string{"add", "--stashed", "--label", plan.Label, "--print-task-id", "--", runnerCommand(plan.OldRunner, plan, true)}
	result, err := client.command(t.Context(), nil, args...)
	if err != nil {
		t.Fatal(err)
	}
	plan.NumericID, err = strconv.ParseInt(strings.TrimSpace(string(result.Stdout)), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if state == StateQueued {
		if _, err = client.command(t.Context(), nil, "enqueue", strconv.FormatInt(plan.NumericID, 10)); err != nil {
			t.Fatal(err)
		}
	}
	return plan
}

func TestRepairConfigNeverUsesExternalShell(t *testing.T) {
	base := canonicalTemp(t)
	source := []byte("client: {}\nshared: {}\ndaemon:\n  shell_command: [/unavailable/user-shell, '-c', '{{ pueue_command_string }}']\n  env_vars: {EDITOR: unsafe, DELEGATE_PUEUE_EDIT_ROOT: unsafe}\n")
	path := filepath.Join(base, "external.yml")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	editor, err := canonicalExecutable(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	derived, err := privateRepairConfig(base, path, task.ComputeSHA256(source), editor)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(derived)
	if err != nil {
		t.Fatal(err)
	}
	config, err := ParseConfig(data)
	if err != nil || config.Daemon.ShellCommand == nil || (*config.Daemon.ShellCommand)[0] != editor || len(config.Daemon.EnvVars) != 0 {
		t.Fatalf("derived repair used user shell/environment: %v %s", err, data)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || string(unchanged) != string(source) {
		t.Fatal(errors.Join(errors.New("external config changed"), err))
	}
}
