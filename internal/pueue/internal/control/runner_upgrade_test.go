package control

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
	job := jobWithOriginalCommand(Job{
		ID: 9, Label: &label, Group: "default", State: StateQueued,
	}, runnerCommand(prior, replacement, false))
	plans, err := planQueuedRunnerCommands(QueueSnapshot{Jobs: []Job{job}}, map[int64]RunnerCommandReplacement{job.ID: replacement})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := plans[job.ID]
	if !ok || plan.expected != originalCommand(job) || plan.replacement != runnerCommand(next, replacement, false) {
		t.Fatalf("repeated upgrade plan = %+v, found=%v", plan, ok)
	}

	job = jobWithOriginalCommand(job, runnerCommand(next, replacement, false))
	plans, err = planQueuedRunnerCommands(QueueSnapshot{Jobs: []Job{job}}, map[int64]RunnerCommandReplacement{job.ID: replacement})
	if err != nil || len(plans) != 0 {
		t.Fatalf("already-upgraded command was not idempotent: plans=%v err=%v", plans, err)
	}

	job = jobWithOriginalCommand(job, runnerCommand(filepath.Join(canonicalTemp(t), "custom-runner"), replacement, false))
	if _, err = planQueuedRunnerCommands(QueueSnapshot{Jobs: []Job{job}}, map[int64]RunnerCommandReplacement{job.ID: replacement}); err == nil {
		t.Fatal("unknown queued runner command was accepted for migration")
	}
}

func TestPlanQueuedRunnerCommandsPreservesSupervisedLaunchMarker(t *testing.T) {
	for _, test := range []struct {
		name       string
		inspection bool
	}{
		{name: "ordinary"},
		{name: "inspection", inspection: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := canonicalTemp(t)
			rootID, taskID := strings.Repeat("c", 32), strings.Repeat("d", 32)
			identity := InspectionIdentity{RootID: rootID, TaskID: taskID, QueueGroup: "default"}
			label := "delegate:" + rootID + ":" + taskID
			if test.inspection {
				label = identity.Label()
			}
			oldRunner := filepath.Join(root, ".supervisor", "delegate-run-"+strings.Repeat("1", 64))
			newRunner := filepath.Join(root, ".supervisor", "delegate-run-"+strings.Repeat("2", 64))
			replacement := RunnerCommandReplacement{
				NumericID: 13, RootID: rootID, TaskID: taskID, Label: label, RootPath: root,
				OldRunner: oldRunner, NewRunner: newRunner, Inspection: test.inspection,
			}
			if test.inspection {
				replacement.InspectionGroup = "default"
			}
			job := jobWithOriginalCommand(Job{
				ID: replacement.NumericID, Label: &label, Group: "default", State: StateQueued,
			}, runnerCommand(oldRunner, replacement, true))

			plans, err := planQueuedRunnerCommands(
				QueueSnapshot{Jobs: []Job{job}},
				map[int64]RunnerCommandReplacement{job.ID: replacement},
			)
			if err != nil {
				t.Fatal(err)
			}
			plan, found := plans[job.ID]
			if !found || plan.expected != originalCommand(job) || plan.replacement != runnerCommand(newRunner, replacement, true) {
				t.Fatalf("supervised runner plan = %+v, found=%v", plan, found)
			}

			job = jobWithOriginalCommand(job, runnerCommand(newRunner, replacement, true))
			plans, err = planQueuedRunnerCommands(
				QueueSnapshot{Jobs: []Job{job}},
				map[int64]RunnerCommandReplacement{job.ID: replacement},
			)
			if err != nil || len(plans) != 0 {
				t.Fatalf("current supervised command was not idempotent: plans=%v err=%v", plans, err)
			}
		})
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
	queued := jobWithOriginalCommand(Job{ID: replacement.NumericID, Label: &label, Group: "default", State: StateQueued}, runnerCommand(oldRunner, replacement, false))
	plans, err := planQueuedRunnerCommands(QueueSnapshot{Jobs: []Job{queued}}, map[int64]RunnerCommandReplacement{queued.ID: replacement})
	if err != nil {
		t.Fatal(err)
	}
	plan := plans[queued.ID]

	alreadyReplaced := queued
	alreadyReplaced = jobWithOriginalCommand(alreadyReplaced, runnerCommand(newRunner, replacement, false))
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

func TestRunnerCommandRepairTreatsStashedAsEditable(t *testing.T) {
	root := canonicalTemp(t)
	rootID, taskID := strings.Repeat("1", 32), strings.Repeat("2", 32)
	label := "delegate:" + rootID + ":" + taskID
	oldRunner := filepath.Join(root, ".supervisor", "delegate-run-"+strings.Repeat("3", 64))
	newRunner := filepath.Join(root, ".supervisor", "delegate-run-"+strings.Repeat("4", 64))
	replacement := RunnerCommandReplacement{
		NumericID: 17, RootID: rootID, TaskID: taskID, Label: label, RootPath: root,
		OldRunner: oldRunner, NewRunner: newRunner,
	}
	stashed := jobWithOriginalCommand(Job{
		ID: replacement.NumericID, Label: &label, Group: "default", State: StateStashed,
	}, runnerCommand(oldRunner, replacement, true))
	plans, err := planQueuedRunnerCommands(
		QueueSnapshot{Jobs: []Job{stashed}},
		map[int64]RunnerCommandReplacement{stashed.ID: replacement},
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, found := plans[stashed.ID]
	if !found || plan.expected != originalCommand(stashed) || plan.replacement != runnerCommand(newRunner, replacement, true) {
		t.Fatalf("stashed runner repair plan = %+v, found=%t", plan, found)
	}
	if complete, completeErr := runnerCommandPlanComplete(QueueSnapshot{Jobs: []Job{stashed}}, plan); complete || completeErr != nil {
		t.Fatalf("unchanged stashed row was treated as repaired: complete=%t err=%v", complete, completeErr)
	}
	if verifyErr := verifyQueuedRunnerCommandPlans(QueueSnapshot{Jobs: []Job{stashed}}, plans); !errors.Is(verifyErr, ErrBinding) {
		t.Fatalf("unrepaired stashed row verification = %v, want binding failure", verifyErr)
	}
	repaired := stashed
	repaired = jobWithOriginalCommand(repaired, plan.replacement)
	if complete, completeErr := runnerCommandPlanComplete(QueueSnapshot{Jobs: []Job{repaired}}, plan); !complete || completeErr != nil {
		t.Fatalf("repaired stashed row was not complete: complete=%t err=%v", complete, completeErr)
	}
	if verifyErr := verifyQueuedRunnerCommandPlans(QueueSnapshot{Jobs: []Job{repaired}}, plans); verifyErr != nil {
		t.Fatalf("repaired stashed row verification: %v", verifyErr)
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
	job := jobWithOriginalCommand(Job{ID: replacement.NumericID, Label: &label, Group: "other", State: StateQueued}, runnerCommand(oldRunner, replacement, false))
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
	job := jobWithOriginalCommand(Job{ID: 11, Label: &label, Group: identity.Group(), State: StateQueued}, runnerCommand(prior, replacement, false))
	plans, err := planQueuedRunnerCommands(QueueSnapshot{Jobs: []Job{job}}, map[int64]RunnerCommandReplacement{job.ID: replacement})
	if err != nil {
		t.Fatal(err)
	}
	plan, ok := plans[job.ID]
	if !ok || !strings.Contains(plan.expected, "--inspection") || plan.replacement != runnerCommand(next, replacement, false) {
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
	job := jobWithOriginalCommand(Job{ID: 11, Label: &label, Group: "default", State: StateQueued}, runnerCommand(prior, replacement, false))
	if _, err := planQueuedRunnerCommands(QueueSnapshot{Jobs: []Job{job}}, map[int64]RunnerCommandReplacement{job.ID: replacement}); !errors.Is(err, ErrBinding) {
		t.Fatalf("inspection migration accepted unexpected group: %v", err)
	}
}
