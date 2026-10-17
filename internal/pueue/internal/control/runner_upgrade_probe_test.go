package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestQueuedRunnerCommandProbeCurrentSharesLease(t *testing.T) {
	fixture := newRunnerUpgradeProbeFixture(t)
	lease := supervisorLeaseTestHold(t, fixture.replacement.RootPath, WithSupervisorLease)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := fixture.supervisor.client.ReplaceQueuedRunnerCommands(ctx, []RunnerCommandReplacement{fixture.replacement})
	supervisorLeaseTestSettleClient(t, fixture.supervisor.client, err)
	if err != nil {
		t.Fatalf("already-current command required exclusive ownership: %v", err)
	}
	assertRunnerUpgradeProbeNoEdits(t, fixture)
	if got := countCommand(readInvocations(t, fixture.supervisor.logPath), "status"); got != 1 {
		t.Fatalf("read-only probe made %d status calls, want one", got)
	}
	lease.finish(t)
}

func TestQueuedRunnerCommandProbeRechecksAfterExclusiveWait(t *testing.T) {
	for _, fresh := range []string{"current", "unexpected", "outdated"} {
		t.Run(fresh, func(t *testing.T) { testRunnerUpgradeProbeRecheck(t, fresh) })
	}
}

func testRunnerUpgradeProbeRecheck(t *testing.T, fresh string) {
	t.Helper()
	fixture := newRunnerUpgradeProbeFixture(t)
	oldCommand := runnerCommand(fixture.replacement.OldRunner, fixture.replacement, false)
	fixture.writeStatus(t, oldCommand)
	lease := supervisorLeaseTestHold(t, fixture.replacement.RootPath, WithSupervisorLease)
	observed := make(chan struct{})
	signalObserved := sync.OnceFunc(func() { close(observed) })
	fixture.supervisor.client.options.Observer = func(event CommandEvent) {
		if event.Stage == "completed" && len(event.Argv) > 3 && event.Argv[3] == "status" {
			signalObserved()
		}
	}
	call := startRunnerUpgradeProbeCall(t, fixture, lease.release)
	select {
	case <-observed:
	case <-call.done:
		t.Fatalf("replacement returned before its shared observation: %v", call.err)
	case <-call.ctx.Done():
		t.Fatalf("replacement never observed the queued command: %v", call.ctx.Err())
	}
	assertRunnerUpgradeProbeCallBlocked(t, call)
	assertRunnerUpgradeProbeNoEdits(t, fixture)
	if got := countCommand(readInvocations(t, fixture.supervisor.logPath), "status"); got != 1 {
		t.Fatalf("replacement took %d snapshots before exclusive ownership, want one", got)
	}

	command, wantErr := oldCommand, error(nil)
	switch fresh {
	case "current":
		command = runnerCommand(fixture.replacement.NewRunner, fixture.replacement, false)
	case "unexpected":
		command, wantErr = "unrelated command at the same numeric ID", ErrBinding
	}
	fixture.writeStatus(t, command)
	lease.finish(t)
	err := call.wait(t)
	if !errors.Is(err, wantErr) {
		t.Fatalf("fresh exclusive observation returned %v, want %v", err, wantErr)
	}
	assertRunnerUpgradeProbeRecheck(t, fixture, fresh)
}

func assertRunnerUpgradeProbeRecheck(t *testing.T, fixture runnerUpgradeProbeFixture, fresh string) {
	t.Helper()
	wantSnapshots, wantEdits := 2, 0
	if fresh == "outdated" {
		wantSnapshots, wantEdits = 3, 1
	} else {
		assertRunnerUpgradeProbeNoEdits(t, fixture)
	}
	commands := readInvocations(t, fixture.supervisor.logPath)
	if countCommand(commands, "status") != wantSnapshots || countCommand(commands, "edit") != wantEdits {
		t.Fatalf("replacement did not replan and apply from its exclusive snapshot: %v", commands)
	}
}

func TestQueuedRunnerCommandProbePreservesErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{
		{name: "snapshot", want: ErrUnknown},
		{name: "command", want: ErrBinding},
		{name: "duplicate", want: ErrConfiguration},
	} {
		t.Run(tc.name, func(t *testing.T) { testRunnerUpgradeProbeError(t, tc.name, tc.want) })
	}
}

func testRunnerUpgradeProbeError(t *testing.T, name string, want error) {
	t.Helper()
	fixture := newRunnerUpgradeProbeFixture(t)
	replacements := []RunnerCommandReplacement{fixture.replacement}
	switch name {
	case "snapshot":
		if err := os.WriteFile(fixture.supervisor.statusPath, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	case "command":
		fixture.writeStatus(t, "unrelated queued command")
	case "duplicate":
		replacements = append(replacements, fixture.replacement)
	}
	lease := supervisorLeaseTestHold(t, fixture.replacement.RootPath, WithSupervisorLease)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := fixture.supervisor.client.ReplaceQueuedRunnerCommands(ctx, replacements)
	supervisorLeaseTestSettleClient(t, fixture.supervisor.client, err)
	if !errors.Is(err, want) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("probe hid an error or deferred it to an exclusive retry: got %v, want %v", err, want)
	}
	assertRunnerUpgradeProbeNoEdits(t, fixture)
	lease.finish(t)
}

func TestQueuedRunnerCommandProbeRetainsPendingLease(t *testing.T) {
	fixture := newRunnerUpgradeProbeFixture(t)
	gate := newStopIdentityLeaseTestGate(t, filepath.Dir(fixture.supervisor.statusPath), "runner-probe-status")
	call := startRunnerUpgradeProbeCall(t, fixture, gate.release)
	gate.wait(t, call.ctx, call.done)
	call.cancel()
	err := call.wait(t)
	pending := pendingFrom(err)
	if !errors.Is(err, ErrInFlight) || !errors.Is(err, ErrUnknown) || pending == nil {
		t.Fatalf("shared probe lost pending observation ownership: %v", err)
	}
	supervisorLeaseTestEnters(t, fixture.replacement.RootPath, WithSupervisorLease)
	supervisorLeaseTestBlocked(t, fixture.replacement.RootPath, WithBootstrapLock)
	gate.release()
	result := waitPending(t, pending)
	if result.Err != nil || !result.Started || result.ExitCode != 0 {
		t.Fatalf("pending status did not finish naturally: %+v", result)
	}
	supervisorLeaseTestEnters(t, fixture.replacement.RootPath, WithBootstrapLock)
	assertRunnerUpgradeProbeNoEdits(t, fixture)
}

type runnerUpgradeProbeFixture struct {
	supervisor  *fakeSupervisor
	replacement RunnerCommandReplacement
}

func newRunnerUpgradeProbeFixture(t *testing.T) runnerUpgradeProbeFixture {
	t.Helper()
	root := canonicalTemp(t)
	base := filepath.Join(root, ".supervisor")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := newFakeSupervisorPaths(t, "ok", "pueue "+FixtureVersion)
	writeExecutable(t, fake.executable, runnerUpgradeProbeScript+fakeSupervisorScript)
	fake.configPath = PrivateConfigPath(root)
	writeFakeConfig(t, fake.configPath, base)
	rootID, taskID := strings.Repeat("a", 32), strings.Repeat("b", 32)
	rootRecord, err := task.MarshalCanonical(task.RootRecord{SchemaVersion: task.SchemaVersion, RootID: rootID, CreatedAt: "2026-01-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "root.json"), rootRecord, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err = WithBootstrapLock(ctx, root, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	fake.client, err = Bind(ctx, fake.executable, fake.configPath, Options{ObservationTimeout: 10 * time.Second, Environment: fake.environment})
	supervisorLeaseTestSettleClient(t, fake.client, err)
	if err != nil {
		t.Fatal(err)
	}
	// Mutating runner repair requires a complete published generation. Use the
	// production publisher so the fixture exercises that same authority gate.
	daemon := filepath.Join(root, "fixture-pueued")
	writeExecutable(t, daemon, privateDaemonFixture)
	if err = attachPrivateDaemon(fake.client, daemon); err != nil {
		t.Fatal(err)
	}
	if err = WithBootstrapLock(ctx, root, func() error {
		return publishPrivateDaemonIdentity(base, fake.client.Binding())
	}); err != nil {
		t.Fatal(err)
	}
	fixture := runnerUpgradeProbeFixture{
		supervisor: fake,
		replacement: RunnerCommandReplacement{
			NumericID: 7, RootID: rootID, TaskID: taskID, Label: "delegate:" + rootID + ":" + taskID,
			RootPath: root, OldRunner: filepath.Join(root, "old-delegate-run"), NewRunner: fake.executable,
		},
	}
	fixture.writeStatus(t, runnerCommand(fixture.replacement.NewRunner, fixture.replacement, false))
	prepareRunnerUpgradeProbeEdit(t, fixture)
	return fixture
}

func (fixture runnerUpgradeProbeFixture) writeStatus(t *testing.T, command string) {
	t.Helper()
	job := statusTestJob(fixture.replacement.NumericID, fixture.replacement.Label, map[string]any{"Queued": map[string]any{"enqueued_at": statusTestTime}})
	job["original_command"], job["command"] = command, command
	data := statusTestPayload(t, map[string]any{strconv.FormatInt(fixture.replacement.NumericID, 10): job}, map[string]any{})
	if err := os.WriteFile(fixture.supervisor.statusPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func prepareRunnerUpgradeProbeEdit(t *testing.T, fixture runnerUpgradeProbeFixture) {
	t.Helper()
	current, err := os.ReadFile(fixture.supervisor.statusPath)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(fixture.supervisor.statusPath)
	for name, data := range map[string][]byte{
		"runner-after-edit.json": current,
		"runner-old-command":     []byte(runnerCommand(fixture.replacement.OldRunner, fixture.replacement, false)),
		"runner-new-command":     []byte(runnerCommand(fixture.replacement.NewRunner, fixture.replacement, false)),
	} {
		if err = os.WriteFile(filepath.Join(base, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

type runnerUpgradeProbeCall struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func startRunnerUpgradeProbeCall(t *testing.T, fixture runnerUpgradeProbeFixture, release func()) *runnerUpgradeProbeCall {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	call := &runnerUpgradeProbeCall{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	t.Cleanup(func() {
		release()
		cancel()
		<-call.done
		for _, pending := range pendingHandles(call.err) {
			waitPending(t, pending)
		}
	})
	go func() {
		call.err = fixture.supervisor.client.ReplaceQueuedRunnerCommands(ctx, []RunnerCommandReplacement{fixture.replacement})
		close(call.done)
	}()
	return call
}

func (call *runnerUpgradeProbeCall) wait(t *testing.T) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case <-call.done:
		return call.err
	case <-ctx.Done():
		t.Fatalf("runner replacement did not return within its observation bound: %v", ctx.Err())
		return ctx.Err()
	}
}

func assertRunnerUpgradeProbeCallBlocked(t *testing.T, call *runnerUpgradeProbeCall) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	select {
	case <-call.done:
		t.Fatalf("outdated command bypassed exclusive ownership: %v", call.err)
	case <-ctx.Done():
	}
}

func assertRunnerUpgradeProbeNoEdits(t *testing.T, fixture runnerUpgradeProbeFixture) {
	t.Helper()
	if commands := readInvocations(t, fixture.supervisor.logPath); countCommand(commands, "edit") != 0 {
		t.Fatalf("read-only runner probe issued an edit: %v", commands)
	}
	editConfig := filepath.Join(filepath.Dir(fixture.supervisor.configPath), "pueue-edit.yml")
	if _, err := os.Lstat(editConfig); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only runner probe materialized edit configuration: %v", err)
	}
}

const runnerUpgradeProbeScript = `#!/bin/sh
set -eu
base=${FAKE_STATUS%/*}
case "${3-}" in
  status)
    if [ -p "$base/runner-probe-status-release" ]; then
      cat "$FAKE_STATUS"
      printf x > "$base/runner-probe-status-seen"
      IFS= read -r release < "$base/runner-probe-status-release"
      exit 0
    fi
    ;;
  edit)
    {
      printf 'argc=%s\n' "$#"
      for arg in "$@"; do printf 'arg=%s\n' "$arg"; done
      printf '%s\n' '---'
    } >> "$FAKE_LOG"
    [ "$4" = "$DELEGATE_PUEUE_EDIT_TASK_ID" ]
    [ "$DELEGATE_PUEUE_EDIT_EXPECTED_COMMAND" = "$(cat "$base/runner-old-command")" ]
    [ "$DELEGATE_PUEUE_EDIT_REPLACEMENT_COMMAND" = "$(cat "$base/runner-new-command")" ]
    cp "$base/runner-after-edit.json" "$FAKE_STATUS"
    exit 0
    ;;
esac
`
