package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
	"github.com/hishamkaram/delegation-layer/internal/workspaces"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

// These cases drive the checkpoint command family over the production
// checkpoint coordinator, record journals, native Git and guardian completion
// composed by checkpointRuntimeFixture. Observations, listing and removal of
// existing attempts run through public Run. Creating a request needs the
// fixture's runtime port, so creation and removal admission call the command
// owner directly with that port; the projection and exit are the same.

// checkpointCLI runs one public checkpoint command and decodes its JSON.
func checkpointCLI(t *testing.T, deps Dependencies, argv ...string) (int, Response, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(append(argv, "--json"), &stdout, &stderr, deps)
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("%v: stdout %q stderr %q: %v", argv, stdout.String(), stderr.String(), err)
	}
	if response.Command != "checkpoint" || response.TaskID != "" || response.Outcome != nil {
		t.Fatalf("%v: checkpoint response borrowed task authority: %+v", argv, response)
	}
	return code, response, stdout.String()
}

func (f checkpointRuntimeFixture) cli(t *testing.T, argv ...string) (int, Response) {
	t.Helper()
	code, response, _ := checkpointCLI(t, f.deps, append([]string{"--root", f.lineage.store.Root, "checkpoint"}, argv...)...)
	return code, response
}

// admit runs creation or removal admission through the command owner with
// the fixture's runtime port.
func (f checkpointRuntimeFixture) admit(t *testing.T, a Arguments) commandResult {
	t.Helper()
	a.Command, a.Root = "checkpoint", f.lineage.store.Root
	return runCheckpointCommandContext(t.Context(), a, f.deps.storeDependencies(), f.options)
}

// requireCheckpoint asserts the exit, IDs and command guidance of a result.
func requireCheckpoint(t *testing.T, label string, code int, response Response, want int, id, operationID, next string) *CheckpointResponse {
	t.Helper()
	checkpoint := response.Checkpoint
	if code != want || checkpoint == nil || checkpoint.ID != id || checkpoint.OperationID != operationID || checkpoint.NextAction != next {
		t.Fatalf("%s = exit %d %+v (error %q); want exit %d id %q operation %q next %q", label, code, checkpoint, response.Error, want, id, operationID, next)
	}
	return checkpoint
}

func requireCurrent(t *testing.T, label string, checkpoint *CheckpointResponse, state string, usable bool) workspaces.CheckpointDescription {
	t.Helper()
	if checkpoint.Current == nil || checkpoint.Current.ID != checkpoint.ID || checkpoint.Current.State != state || checkpoint.Current.Usable != usable {
		t.Fatalf("%s current = %+v; want state %s usable %t", label, checkpoint.Current, state, usable)
	}
	return *checkpoint.Current
}

// createQualifiedThroughCommands accepts an ordinary creation through the
// command owner, runs its helper and observes the qualified checkpoint.
func (f checkpointRuntimeFixture) createQualifiedThroughCommands(t *testing.T) string {
	t.Helper()
	created := f.admit(t, Arguments{CheckpointAction: "create", Cwd: f.lineage.main})
	checkpointID := created.response.Checkpoint.ID
	checkpoint := requireCheckpoint(t, "accepted create", created.code, created.response, 3, checkpointID, "", checkpointWatch)
	if checkpointID == "" || created.err != nil || checkpoint.Current != nil || checkpoint.ReasonCode != "" {
		t.Fatalf("accepted create = %+v, %v", checkpoint, created.err)
	}
	f.requireAdds(t, 1)
	// The fixture supervisor cannot observe the submitted job, so the pending
	// marker is joined with a concrete observation fault: never pending only.
	code, response := f.cli(t, "status", checkpointID)
	if unobserved := requireCheckpoint(t, "unobservable helper", code, response, 1, "", "", checkpointStopAndReport); unobserved.Current != nil ||
		!strings.Contains(response.Error, "pending") || !strings.Contains(response.Error, "unknown") {
		t.Fatalf("unobservable helper = %+v, error %q", unobserved, response.Error)
	}
	f.runHelper(t, checkpointID, true)
	code, response = f.cli(t, "status", checkpointID, "--watch", "5s")
	current := requireCurrent(t, "created status", requireCheckpoint(t, "created status", code, response, 0, checkpointID, "", ""), workspace.CheckpointCheckpointed, true)
	if current.CommitOID == "" || current.SnapshotRef == "" || current.Cwd != f.lineage.main || current.WorkspaceID != "" {
		t.Fatalf("qualified ordinary checkpoint = %+v", current)
	}
	f.requireCheckpointRef(t, current)
	f.requireAdds(t, 1)
	return checkpointID
}

func TestCheckpointOrdinaryLifecycleThroughCommands(t *testing.T) {
	fixture := newCheckpointRuntimeFixture(t)
	checkpointID := fixture.createQualifiedThroughCommands(t)
	removal := fixture.admit(t, Arguments{CheckpointAction: "remove", CheckpointID: checkpointID})
	removalID := removal.response.Checkpoint.OperationID
	accepted := requireCheckpoint(t, "accepted removal", removal.code, removal.response, 3, checkpointID, removalID, checkpointWatch)
	if removalID == "" || removalID == checkpointID || accepted.Operation != nil || accepted.Current != nil {
		t.Fatalf("accepted removal = %+v", accepted)
	}
	fixture.requireAdds(t, 2)
	fixture.runHelper(t, removalID, true)
	code, response := fixture.cli(t, "status", checkpointID, "--operation", removalID)
	exact := requireCheckpoint(t, "exact removal", code, response, 0, checkpointID, removalID, "")
	if exact.Operation == nil || *exact.Operation != (workspaces.CheckpointRemovalDescription{OperationID: removalID, Attempts: 1, State: workspace.CheckpointRemoved}) {
		t.Fatalf("exact removal operation = %+v", exact.Operation)
	}
	code, response = fixture.cli(t, "status", checkpointID)
	history := requireCurrent(t, "removed status", requireCheckpoint(t, "removed status", code, response, 0, checkpointID, "", ""), workspace.CheckpointCheckpointed, false)
	if history.Removal == nil || history.Removal.OperationID != removalID || history.Removal.State != workspace.CheckpointRemoved {
		t.Fatalf("historical creation lost its removal: %+v", history)
	}
	code, response = fixture.cli(t, "remove", checkpointID)
	requireCheckpoint(t, "repeated removal", code, response, 0, checkpointID, removalID, "")
	fixture.requireAdds(t, 2)
	if attempts := fixture.attempts(t, checkpointID); len(attempts) != 1 {
		t.Fatalf("repeated removal registered another attempt: %+v", attempts)
	}
}

func TestCheckpointSelectedFailedRemovalKeepsItsOwnExit(t *testing.T) {
	fixture := newCheckpointRuntimeFixture(t)
	checkpointID := fixture.beginOrdinary(t, fixture.lineage.main)
	fixture.runHelper(t, checkpointID, true)
	failedID := fixture.admit(t, Arguments{CheckpointAction: "remove", CheckpointID: checkpointID}).response.Checkpoint.OperationID
	fixture.runHelper(t, failedID, false)

	code, response := fixture.cli(t, "status", checkpointID, "--operation", failedID)
	failed := requireCheckpoint(t, "failed attempt", code, response, 1, checkpointID, failedID, checkpointStopAndReport)
	if failed.Operation == nil || failed.Operation.State != workspace.CheckpointUnavailable || failed.Operation.ReasonCode != workspace.CheckpointRemovalResultUnavailable ||
		response.Error != errCheckpointRemovalUnavailable.Error() {
		t.Fatalf("failed attempt = %+v, error %q", failed.Operation, response.Error)
	}
	code, response = fixture.cli(t, "status", checkpointID)
	history := requireCurrent(t, "failed history", requireCheckpoint(t, "failed history", code, response, 0, checkpointID, "", checkpointStopAndReport), workspace.CheckpointCheckpointed, false)
	if history.Removal == nil || history.Removal.State != workspace.CheckpointUnavailable || response.Error != "" {
		t.Fatalf("failed history = %+v, error %q", history, response.Error)
	}

	retry := fixture.admit(t, Arguments{CheckpointAction: "remove", CheckpointID: checkpointID})
	retryID := retry.response.Checkpoint.OperationID
	requireCheckpoint(t, "retried removal", retry.code, retry.response, 3, checkpointID, retryID, checkpointWatch)
	if retryID == failedID {
		t.Fatalf("retry borrowed the failed attempt %s", failedID)
	}
	code, response = fixture.cli(t, "status", checkpointID, "--operation", failedID)
	if failed = requireCheckpoint(t, "historical failure beside pending retry", code, response, 1, checkpointID, failedID, checkpointStopAndReport); failed.Current.Removal.OperationID != retryID {
		t.Fatalf("latest removal = %+v, want pending retry %s", failed.Current.Removal, retryID)
	}
	fixture.runHelper(t, retryID, true)
	code, response = fixture.cli(t, "status", checkpointID, "--operation", failedID)
	requireCheckpoint(t, "historical failure beside removed retry", code, response, 1, checkpointID, failedID, checkpointStopAndReport)
	code, response = fixture.cli(t, "status", checkpointID, "--operation", retryID)
	if exact := requireCheckpoint(t, "removed retry", code, response, 0, checkpointID, retryID, ""); exact.Operation.Attempts != 2 {
		t.Fatalf("retry attempts = %+v", exact.Operation)
	}
}

func TestCheckpointOperationSelectorAuthenticatesRemovalOnly(t *testing.T) {
	fixture := newCheckpointRuntimeFixture(t)
	first := fixture.beginOrdinary(t, fixture.lineage.main)
	fixture.runHelper(t, first, true)
	second := fixture.beginOrdinary(t, fixture.lineage.main)
	fixture.runHelper(t, second, true)
	foreign := fixture.admit(t, Arguments{CheckpointAction: "remove", CheckpointID: second}).response.Checkpoint.OperationID
	for name, selector := range map[string]string{"creation ID": first, "another checkpoint's removal": foreign, "unknown operation": strings.Repeat("e", 32)} {
		code, response := fixture.cli(t, "status", first, "--operation", selector)
		checkpoint := requireCheckpoint(t, name, code, response, 1, "", "", checkpointStopAndReport)
		if checkpoint.Current != nil || checkpoint.Operation != nil || response.Error == "" {
			t.Fatalf("%s selector borrowed facts: %+v", name, checkpoint)
		}
	}
	code, response := fixture.cli(t, "status", first)
	requireCurrent(t, "unselected checkpoint", requireCheckpoint(t, "unselected checkpoint", code, response, 0, first, "", ""), workspace.CheckpointCheckpointed, true)
}

func TestCheckpointRemovalBeforeQualificationIsRefusedWithoutAttempt(t *testing.T) {
	fixture := newCheckpointRuntimeFixture(t)
	checkpointID := fixture.beginOrdinary(t, fixture.lineage.main)
	refused := fixture.admit(t, Arguments{CheckpointAction: "remove", CheckpointID: checkpointID})
	checkpoint := requireCheckpoint(t, "removal of a pending creation", refused.code, refused.response, 3, "", "", checkpointRetryPrefix+checkpointRemoveAction)
	if checkpoint.ReasonCode != checkpointNotAuthorized || checkpoint.Current != nil || refused.err != nil {
		t.Fatalf("refused removal = %+v, %v", checkpoint, refused.err)
	}
	if attempts := fixture.attempts(t, checkpointID); len(attempts) != 0 {
		t.Fatalf("refused removal registered %+v", attempts)
	}
	fixture.requireAdds(t, 1)
}

func TestCheckpointManagedCreationRefusesBusyUseOwner(t *testing.T) {
	fixture := newCheckpointRuntimeFixture(t)
	resource, _ := fixture.lineage.provisionedChild(t, fixture.lineage.main)
	workspaceID := resource.Request().WorkspaceID
	owner, err := resource.TryAcquireUseOwner(fixture.ctx())
	if err != nil {
		t.Fatal(err)
	}
	refused := fixture.admit(t, Arguments{CheckpointAction: "create", WorkspaceID: workspaceID})
	checkpoint := requireCheckpoint(t, "busy managed create", refused.code, refused.response, 3, "", "", checkpointRetryPrefix+checkpointCreateAction)
	if checkpoint.ReasonCode != checkpointNotAuthorized || refused.err != nil {
		t.Fatalf("busy managed create = %+v, %v", checkpoint, refused.err)
	}
	fixture.requireAdds(t, 0)
	if err = owner.Release(); err != nil {
		t.Fatal(err)
	}
	accepted := fixture.admit(t, Arguments{CheckpointAction: "create", WorkspaceID: workspaceID})
	checkpointID := accepted.response.Checkpoint.ID
	requireCheckpoint(t, "managed create", accepted.code, accepted.response, 3, checkpointID, "", checkpointWatch)
	fixture.runHelper(t, checkpointID, true)
	code, response := fixture.cli(t, "status", checkpointID)
	if current := requireCurrent(t, "managed status", requireCheckpoint(t, "managed status", code, response, 0, checkpointID, "", ""), workspace.CheckpointCheckpointed, true); current.WorkspaceID != workspaceID || current.Cwd != "" {
		t.Fatalf("managed checkpoint = %+v", current)
	}
}

func TestCheckpointHelpListsCommandFamily(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-initialized")
	assertCheckpointHelpCommand(t, checkpointHelpOutput(t, root, "help"), "checkpoint")
	group := checkpointHelpOutput(t, root, "checkpoint", "--help")
	if !strings.Contains(group, "Usage: delegate checkpoint COMMAND [flags]") {
		t.Fatalf("checkpoint family routing is not discoverable: %s", group)
	}
	for _, fixture := range []struct {
		action, usage string
		facts         []string
	}{
		{"create", "Usage: delegate checkpoint create [flags]", []string{
			"Required: exactly one of --cwd ABS or --workspace ID", "--cwd", "absolute ordinary Git checkout",
			"--workspace", "managed workspace ID", "--budget", "finite helper budget (default: 5m)",
			"3 no final creation receipt", "reason_code", "next_action",
		}},
		{"status", "Usage: delegate checkpoint status CHECKPOINT_ID [flags]", []string{
			"--operation", "acknowledged removal operation ID", "--watch", "finite checkpoint observation bound",
			"default: 0s", "3 checkpoint observation pending",
		}},
		{"list", "Usage: delegate checkpoint list [flags]", []string{
			"--after", "checkpoint cursor from a previous page", "--limit", "checkpoints per page", "default: 50",
		}},
		{"remove", "Usage: delegate checkpoint remove CHECKPOINT_ID [flags]", []string{
			"--budget", "finite helper budget (default: 5m)", "3 no final removal receipt", "acknowledged IDs",
		}},
	} {
		assertCheckpointHelpCommand(t, group, fixture.action)
		t.Run(fixture.action, func(t *testing.T) {
			help := checkpointHelpOutput(t, root, "checkpoint", fixture.action, "--help")
			for _, fact := range append([]string{fixture.usage, "--root", "--json"}, fixture.facts...) {
				if !strings.Contains(help, fact) {
					t.Errorf("checkpoint %s help omits %q: %s", fixture.action, fact, help)
				}
			}
			if strings.Contains(help, "TASK_ID") {
				t.Fatalf("checkpoint help borrowed task identity: %s", help)
			}
		})
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("help initialized execution state: %v", err)
	}
}

func checkpointHelpOutput(t *testing.T, root string, argv ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(append([]string{"--root", root}, argv...), &stdout, &stderr, Dependencies{})
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("help %v: exit=%d stderr=%q", argv, code, stderr.String())
	}
	return stdout.String()
}

func assertCheckpointHelpCommand(t *testing.T, text, command string) {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[0] == command {
			return
		}
	}
	t.Fatalf("help does not list command %s: %s", command, text)
}

func TestCheckpointCommandsRefuseMissingRootWithoutInitializing(t *testing.T) {
	id := strings.Repeat("a", 32)
	for _, argv := range [][]string{{"list"}, {"status", id}, {"status", id, "--operation", id}, {"remove", id}, {"create", "--workspace", id}} {
		root := filepath.Join(t.TempDir(), "missing")
		code, response, _ := checkpointCLI(t, Dependencies{}, append([]string{"--root", root, "checkpoint"}, argv...)...)
		requireCheckpoint(t, strings.Join(argv, " "), code, response, 1, "", "", checkpointStopAndReport)
		if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%v initialized the missing root: %v", argv, err)
		}
	}
}

func TestCheckpointListOfEmptyRootEmitsEmptyPage(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	code, response, raw := checkpointCLI(t, Dependencies{}, "--root", store.Root, "checkpoint", "list")
	if code != 0 || response.Checkpoint != nil || response.Checkpoints == nil || len(*response.Checkpoints) != 0 || response.CheckpointCursor != "" ||
		!strings.Contains(raw, `"checkpoints":[]`) || response.RootID != store.RootID {
		t.Fatalf("empty list = exit %d %s", code, raw)
	}
	_, response, raw = checkpointCLI(t, Dependencies{}, "--root", store.Root, "checkpoint", "status", strings.Repeat("a", 32))
	if response.Checkpoints != nil || strings.Contains(raw, `"checkpoints"`) {
		t.Fatalf("status emitted a list page: %s", raw)
	}
}

// TestCheckpointListReadsMetadataOnly lists qualified, unavailable and
// pending creations through public Run without writing, skips only a cleanly
// absent request and fails on a later unloadable one.
func TestCheckpointListReadsMetadataOnly(t *testing.T) {
	fixture := newCheckpointRuntimeFixture(t)
	store := fixture.lineage.store
	qualified := fixture.beginOrdinary(t, fixture.lineage.main)
	fixture.runHelper(t, qualified, true)
	unavailable := fixture.beginOrdinary(t, fixture.lineage.main)
	fixture.runHelper(t, unavailable, false)
	pending := fixture.beginOrdinary(t, fixture.lineage.main)
	workspacefixture.MakeBareOperationDirectory(t, store, strings.Repeat("8", 32))
	// Every checkpoint and removal record lives under the operation journal.
	journal := filepath.Dir(workspacefixture.OperationDirectory(store, qualified))
	before := supervisorfixture.SnapshotStore(t, journal)

	states := map[string]string{}
	for argv := []string{"list", "--limit", "1"}; ; {
		code, response := fixture.cli(t, argv...)
		if code != 0 || response.Checkpoints == nil || len(*response.Checkpoints) != 1 || response.Checkpoint != nil {
			t.Fatalf("list %v = exit %d %+v", argv, code, response)
		}
		item := (*response.Checkpoints)[0]
		states[item.ID] = item.State
		cursor := response.CheckpointCursor
		if cursor == "" {
			break
		}
		if cursor != item.ID {
			t.Fatalf("cursor %q does not name the page's last checkpoint %q", cursor, item.ID)
		}
		argv = []string{"list", "--limit", "1", "--after", cursor}
	}
	want := map[string]string{qualified: workspace.CheckpointCheckpointed, unavailable: workspace.CheckpointUnavailable, pending: workspaces.CheckpointPending}
	if !reflect.DeepEqual(states, want) {
		t.Fatalf("listed states = %v, want %v", states, want)
	}
	if after := supervisorfixture.SnapshotStore(t, journal); !reflect.DeepEqual(after, before) {
		t.Fatal("checkpoint list changed the operation journal")
	}
	fixture.requireAdds(t, 3)

	malformed := strings.Repeat("f", 32)
	workspacefixture.MakeBareOperationDirectory(t, store, malformed)
	workspacefixture.WriteOperationFile(t, store, malformed, "request.json", []byte("{"))
	code, response := fixture.cli(t, "list")
	if requireCheckpoint(t, "unloadable request", code, response, 1, "", "", checkpointStopAndReport); response.Checkpoints != nil || response.Error == "" {
		t.Fatalf("unloadable request = %+v", response)
	}
}

func TestCheckpointStatusObservesUnavailableCreationSuccessfully(t *testing.T) {
	fixture := newCheckpointRuntimeFixture(t)
	checkpointID := fixture.beginOrdinary(t, fixture.lineage.main)
	fixture.runHelper(t, checkpointID, false)
	code, response := fixture.cli(t, "status", checkpointID)
	current := requireCurrent(t, "unavailable creation", requireCheckpoint(t, "unavailable creation", code, response, 0, checkpointID, "", checkpointStopAndReport), workspace.CheckpointUnavailable, false)
	if current.ReasonCode != workspace.CheckpointResultUnavailable || response.Error != "" {
		t.Fatalf("unavailable creation = %+v, error %q", current, response.Error)
	}
	if ids, err := workspace.ResourceIDs(fixture.ctx(), fixture.lineage.store); err != nil || len(ids) != 0 {
		t.Fatalf("ordinary checkpoint touched managed resources: %v, %v", ids, err)
	}
}
