package pueue

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestEscapePueueArgumentRoundTripsShellValues(t *testing.T) {
	for _, value := range []string{"delegate-run", "path with spaces", "quote'and!bang", "café", "line\nbreak", ""} {
		t.Run(value, func(t *testing.T) {
			command := "printf '%s' " + escapePueueArgument(value)
			output, err := exec.Command("/bin/sh", "-c", command).Output()
			if err != nil {
				t.Fatal(err)
			}
			if string(output) != value {
				t.Fatalf("escaped value round trip = %q, want %q", output, value)
			}
		})
	}
}

func TestPrivateEditConfigPublishesFilesModeCreateOnce(t *testing.T) {
	base := canonicalTemp(t)
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(base, "pueue.yml")
	source := []byte(privateConfigYAML(base))
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := task.ComputeSHA256(source)
	path, err := privateEditConfig(base, sourcePath, digest)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(base, "pueue-edit.yml") {
		t.Fatalf("edit config path = %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseConfig(data)
	if err != nil || parsed.Client.EditMode != "files" {
		t.Fatalf("edit config did not select file mode: %s", data)
	}
	if _, err = privateEditConfig(base, sourcePath, digest); err != nil {
		t.Fatalf("identical create-once edit config was rejected: %v", err)
	}
	if err = os.WriteFile(path, []byte("client: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = privateEditConfig(base, sourcePath, digest); err == nil {
		t.Fatal("conflicting edit config was overwritten")
	}
}

func TestPrivateEditConfigPreservesBoundPrivateSettings(t *testing.T) {
	base := canonicalTemp(t)
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(base, "pueue.yml")
	source := strings.Replace(privateConfigYAML(base), "  unix_socket_path: "+strconv.Quote(filepath.Join(base, "run", "pueue.sock")), "  unix_socket_path: "+strconv.Quote(filepath.Join(base, "custom.sock")), 1)
	source = strings.Replace(source, "client:\n", "client:\n  edit_mode: toml\n", 1)
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := ParseConfig([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	digest := task.ComputeSHA256([]byte(source))
	path, err := privateEditConfig(base, sourcePath, digest)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := ParseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if after.Client.EditMode != "files" || !reflect.DeepEqual(after.Shared, before.Shared) || !reflect.DeepEqual(after.Daemon, before.Daemon) {
		t.Fatalf("edit config changed bound settings: before=%+v after=%+v", before, after)
	}
	if _, err = privateEditConfig(base, sourcePath, strings.Repeat("0", 64)); err == nil {
		t.Fatal("changed supervisor config digest was accepted")
	}
}

func TestPrivateEditConfigDereferencesClientAlias(t *testing.T) {
	base := canonicalTemp(t)
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	source := strings.Replace(privateConfigYAML(base), "client:\n  show_confirmation_questions: false\n", "profiles:\n  base:\n    client: &base\n      show_confirmation_questions: false\n      edit_mode: toml\nclient: *base\n", 1)
	sourcePath := filepath.Join(base, "pueue.yml")
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	var before rawSettings
	if err := yaml.Unmarshal([]byte(source), &before); err != nil {
		t.Fatal(err)
	}
	path, err := privateEditConfig(base, sourcePath, task.ComputeSHA256([]byte(source)))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var after rawSettings
	if err = yaml.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	if after.Client == nil || after.Client.EditMode == nil || *after.Client.EditMode != "files" {
		t.Fatalf("top-level aliased client did not select file mode: %s", data)
	}
	if before.Profiles["base"].Client.EditMode == nil || *before.Profiles["base"].Client.EditMode != "toml" || after.Profiles["base"].Client.EditMode == nil || *after.Profiles["base"].Client.EditMode != "toml" {
		t.Fatalf("dereferencing top-level client changed its profile source: before=%+v after=%+v", before.Profiles["base"].Client, after.Profiles["base"].Client)
	}
}

func TestPrivateEditConfigDetachesAnchoredTopLevelClient(t *testing.T) {
	base := canonicalTemp(t)
	source := strings.Replace(privateConfigYAML(base), "client:\n  show_confirmation_questions: false\n", "client: &base\n  show_confirmation_questions: false\n  edit_mode: toml\nprofiles:\n  base:\n    client: *base\n", 1)
	sourcePath := filepath.Join(base, "pueue.yml")
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	var before rawSettings
	if err := yaml.Unmarshal([]byte(source), &before); err != nil {
		t.Fatal(err)
	}
	path, err := privateEditConfig(base, sourcePath, task.ComputeSHA256([]byte(source)))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var after rawSettings
	if err = yaml.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	if after.Client == nil || after.Client.EditMode == nil || *after.Client.EditMode != "files" {
		t.Fatalf("top-level anchored client did not select file mode: %s", data)
	}
	if before.Profiles["base"].Client.EditMode == nil || *before.Profiles["base"].Client.EditMode != "toml" || after.Profiles["base"].Client.EditMode == nil || *after.Profiles["base"].Client.EditMode != "toml" {
		t.Fatalf("changing the top-level client changed its profile source: before=%+v after=%+v", before.Profiles["base"].Client, after.Profiles["base"].Client)
	}
}

func TestPrivateEditConfigRewritesEditModeScalarAlias(t *testing.T) {
	base := canonicalTemp(t)
	source := strings.Replace(privateConfigYAML(base), "client:\n  show_confirmation_questions: false\n", "profiles:\n  base:\n    client:\n      edit_mode: &mode toml\nclient:\n  show_confirmation_questions: false\n  edit_mode: *mode\n", 1)

	before, err := ParseConfig([]byte(source))
	if err != nil {
		t.Fatalf("fixture config is invalid: %v", err)
	}
	data, err := configWithFileEditMode([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	after, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("migrated config is invalid: %v\n%s", err, data)
	}
	if after.Client.EditMode != "files" {
		t.Fatalf("migrated client edit mode = %q, want files", after.Client.EditMode)
	}
	var afterRaw rawSettings
	if err = yaml.Unmarshal(data, &afterRaw); err != nil {
		t.Fatal(err)
	}
	if before.Client.EditMode != "toml" || afterRaw.Profiles["base"].Client.EditMode == nil || *afterRaw.Profiles["base"].Client.EditMode != "toml" {
		t.Fatalf("changing the client alias changed profile config: before=%+v after=%+v", before, afterRaw.Profiles["base"].Client)
	}
}

func TestPlanQueuedRunnerCommandsAcceptsRepeatedManagedUpgrade(t *testing.T) {
	root := canonicalTemp(t)
	rootID, taskID := strings.Repeat("a", 32), strings.Repeat("b", 32)
	label := "delegate:" + rootID + ":" + taskID
	prior := filepath.Join(root, ".supervisor", "delegate-run-"+strings.Repeat("1", 64))
	next := filepath.Join(root, ".supervisor", "delegate-run-"+strings.Repeat("2", 64))
	replacement := RunnerCommandReplacement{
		NumericID: 9, RootID: rootID, TaskID: taskID, Label: label, RootPath: root,
		OldRunner: filepath.Join(canonicalTemp(t), "delegate-run"), PreviousManagedRunners: []string{prior}, NewRunner: next,
	}
	job := Job{
		ID: 9, Label: &label, Group: "default", State: StateQueued,
		originalCommand: runnerCommand(prior, replacement),
	}
	plans, err := planQueuedRunnerCommands(QueueSnapshot{Jobs: []Job{job}}, map[int64]RunnerCommandReplacement{job.ID: replacement})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := plans[job.ID]
	if !ok || plan.expected != job.originalCommand || plan.replacement != runnerCommand(next, replacement) {
		t.Fatalf("repeated upgrade plan = %+v, found=%v", plan, ok)
	}

	job.originalCommand = runnerCommand(next, replacement)
	plans, err = planQueuedRunnerCommands(QueueSnapshot{Jobs: []Job{job}}, map[int64]RunnerCommandReplacement{job.ID: replacement})
	if err != nil || len(plans) != 0 {
		t.Fatalf("already-upgraded command was not idempotent: plans=%v err=%v", plans, err)
	}

	job.originalCommand = runnerCommand(filepath.Join(canonicalTemp(t), "custom-runner"), replacement)
	if _, err = planQueuedRunnerCommands(QueueSnapshot{Jobs: []Job{job}}, map[int64]RunnerCommandReplacement{job.ID: replacement}); err == nil {
		t.Fatal("unknown queued runner command was accepted for migration")
	}
}

func TestRunnerCommandPlanCompleteReconcilesConcurrentQueueChanges(t *testing.T) {
	root := canonicalTemp(t)
	rootID, taskID := strings.Repeat("e", 32), strings.Repeat("f", 32)
	label := "delegate:" + rootID + ":" + taskID
	oldRunner := filepath.Join(root, ".supervisor", "delegate-run-"+strings.Repeat("1", 64))
	newRunner := filepath.Join(root, ".supervisor", "delegate-run-"+strings.Repeat("2", 64))
	replacement := RunnerCommandReplacement{
		NumericID: 12, RootID: rootID, TaskID: taskID, Label: label, RootPath: root,
		OldRunner: filepath.Join(canonicalTemp(t), "delegate-run"), PreviousManagedRunners: []string{oldRunner}, NewRunner: newRunner,
	}
	queued := Job{ID: replacement.NumericID, Label: &label, Group: "default", State: StateQueued, originalCommand: runnerCommand(oldRunner, replacement)}
	plans, err := planQueuedRunnerCommands(QueueSnapshot{Jobs: []Job{queued}}, map[int64]RunnerCommandReplacement{queued.ID: replacement})
	if err != nil {
		t.Fatal(err)
	}
	plan := plans[queued.ID]

	alreadyReplaced := queued
	alreadyReplaced.originalCommand = runnerCommand(newRunner, replacement)
	if complete, completeErr := runnerCommandPlanComplete(QueueSnapshot{Jobs: []Job{alreadyReplaced}}, plan); completeErr != nil || !complete {
		t.Fatalf("concurrent successful replacement was not reconciled: complete=%t err=%v", complete, completeErr)
	}

	started := queued
	started.State = StateRunning
	if complete, completeErr := runnerCommandPlanComplete(QueueSnapshot{Jobs: []Job{started}}, plan); completeErr != nil || !complete {
		t.Fatalf("job transition to running was not reconciled: complete=%t err=%v", complete, completeErr)
	}

	if complete, completeErr := runnerCommandPlanComplete(QueueSnapshot{Jobs: []Job{queued}}, plan); complete || completeErr != nil {
		t.Fatalf("unchanged queued task was treated as repaired: complete=%t err=%v", complete, completeErr)
	}
}

func TestReplaceQueuedRunnerCommandsSerializesOnBootstrapLock(t *testing.T) {
	root := canonicalTemp(t)
	base := filepath.Join(root, ".supervisor")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := filepath.Join(canonicalTemp(t), "delegate-run")
	if err := os.WriteFile(runner, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	rootID, taskID := strings.Repeat("a", 32), strings.Repeat("b", 32)
	rootRecord, err := task.MarshalCanonical(task.RootRecord{SchemaVersion: task.SchemaVersion, RootID: rootID, CreatedAt: "2026-01-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "root.json"), rootRecord, 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := PrivateConfigPath(root)
	client := &Client{
		binding:        task.SupervisorRef{ConfigPath: configPath},
		runtimeBinding: task.SupervisorRef{ConfigPath: configPath},
	}
	replacement := RunnerCommandReplacement{
		NumericID: 1, RootID: rootID, TaskID: taskID,
		Label: "delegate:" + rootID + ":" + taskID, RootPath: root,
		OldRunner: runner, NewRunner: runner,
	}
	heldLock, err := acquireBootstrapLock(context.Background(), filepath.Join(base, "bootstrap.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := closeBootstrapLock(heldLock); closeErr != nil {
			t.Error(closeErr)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err = client.ReplaceQueuedRunnerCommands(ctx, []RunnerCommandReplacement{replacement})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runner repair did not wait within the caller's deadline: %v", err)
	}
}

func TestPlanQueuedRunnerCommandsRejectsUnexpectedOrdinaryGroup(t *testing.T) {
	root := canonicalTemp(t)
	rootID, taskID := strings.Repeat("a", 32), strings.Repeat("b", 32)
	label := "delegate:" + rootID + ":" + taskID
	oldRunner := filepath.Join(root, ".supervisor", "delegate-run-"+strings.Repeat("1", 64))
	replacement := RunnerCommandReplacement{
		NumericID: 9, RootID: rootID, TaskID: taskID, Label: label, RootPath: root,
		OldRunner: oldRunner, NewRunner: filepath.Join(root, ".supervisor", "delegate-run-"+strings.Repeat("2", 64)),
	}
	job := Job{ID: replacement.NumericID, Label: &label, Group: "other", State: StateQueued, originalCommand: runnerCommand(oldRunner, replacement)}
	if _, err := planQueuedRunnerCommands(QueueSnapshot{Jobs: []Job{job}}, map[int64]RunnerCommandReplacement{job.ID: replacement}); !errors.Is(err, ErrBinding) {
		t.Fatalf("ordinary migration accepted job from another group: %v", err)
	}
}

func TestPlanQueuedInspectionRunnerCommandPreservesWorkerArguments(t *testing.T) {
	root := canonicalTemp(t)
	identity := InspectionIdentity{RootID: strings.Repeat("c", 32), TaskID: strings.Repeat("d", 32)}
	label := identity.Label()
	prior := filepath.Join(root, ".supervisor", "delegate-run-"+strings.Repeat("3", 64))
	next := filepath.Join(root, ".supervisor", "delegate-run-"+strings.Repeat("4", 64))
	replacement := RunnerCommandReplacement{
		NumericID: 11, RootID: identity.RootID, TaskID: identity.TaskID, Label: label,
		RootPath: root, OldRunner: filepath.Join(canonicalTemp(t), "delegate-run"),
		PreviousManagedRunners: []string{prior}, NewRunner: next, Inspection: true,
	}
	job := Job{ID: 11, Label: &label, Group: identity.Group(), State: StateQueued, originalCommand: runnerCommand(prior, replacement)}
	plans, err := planQueuedRunnerCommands(QueueSnapshot{Jobs: []Job{job}}, map[int64]RunnerCommandReplacement{job.ID: replacement})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := plans[job.ID]
	if !ok || !strings.Contains(plan.expected, "--inspection") || plan.replacement != runnerCommand(next, replacement) {
		t.Fatalf("inspection migration plan = %+v, found=%v", plan, ok)
	}
}

func TestPlanQueuedInspectionRunnerCommandRejectsUnexpectedGroup(t *testing.T) {
	root := canonicalTemp(t)
	identity := InspectionIdentity{RootID: strings.Repeat("c", 32), TaskID: strings.Repeat("d", 32)}
	label := identity.Label()
	prior := filepath.Join(root, ".supervisor", "delegate-run-"+strings.Repeat("3", 64))
	replacement := RunnerCommandReplacement{
		NumericID: 11, RootID: identity.RootID, TaskID: identity.TaskID, Label: label,
		RootPath: root, OldRunner: filepath.Join(canonicalTemp(t), "delegate-run"),
		NewRunner: filepath.Join(root, ".supervisor", "delegate-run-"+strings.Repeat("4", 64)), Inspection: true,
	}
	job := Job{ID: 11, Label: &label, Group: "default", State: StateQueued, originalCommand: runnerCommand(prior, replacement)}
	if _, err := planQueuedRunnerCommands(QueueSnapshot{Jobs: []Job{job}}, map[int64]RunnerCommandReplacement{job.ID: replacement}); !errors.Is(err, ErrBinding) {
		t.Fatalf("inspection migration accepted unexpected group: %v", err)
	}
}
