package app

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

// nativeWorkspaceGitRunner gives each owned Git command its own production
// launch-wrapper scope through launch, which is bound to the fixture's finite
// deadline because native scope creation takes no context. It owns no Start,
// Wait, timer or signal path; the Git boundary checks the caller context
// before every command.
type nativeWorkspaceGitRunner struct{ launch func(*exec.Cmd) error }

func (r nativeWorkspaceGitRunner) Run(ctx context.Context, command *exec.Cmd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.launch(command)
}

// nativeLineage is one invocation-owned repository, state root and shared
// workspace container. Every path lives below the test's temporary root.
type nativeLineage struct {
	scope     worktreeGateTestScope
	git       workspace.Git
	root      string
	main      string
	store     *taskdir.Store
	container string
}

func newNativeLineage(t *testing.T) nativeLineage {
	t.Helper()
	return newNativeLineageFormat(t, "sha1")
}

// newNativeLineageFormat initializes the main repository with objectFormat.
func newNativeLineageFormat(t *testing.T, objectFormat string) nativeLineage {
	t.Helper()
	scope := nativeWorktreeGateScope(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	t.Cleanup(cancel)
	scope.ctx = ctx
	root := supervisorfixture.CanonicalTempDir(t)
	launch := func(command *exec.Cmd) error { return scope.Run(command, execution.CommandHooks{}) }
	lineage := nativeLineage{scope: scope, git: workspace.Git{Runner: nativeWorkspaceGitRunner{launch: launch}}, root: root, main: filepath.Join(root, "main")}
	if err := os.Mkdir(lineage.main, 0o700); err != nil {
		t.Fatal(err)
	}
	lineage.gitIn(t, lineage.main, "init", "--quiet", "--object-format="+objectFormat)
	writeLineageFile(t, filepath.Join(lineage.main, "tracked.txt"), "base\n")
	writeLineageFile(t, filepath.Join(lineage.main, "removed.txt"), "remove me\n")
	lineage.gitIn(t, lineage.main, "add", "-A")
	lineage.commit(t, lineage.main, "base")
	lineage.store = openLineageStore(t, filepath.Join(root, "state"))
	lineage.container = workspace.ContainerPath(lineage.store.Root, lineage.store.RootID)
	return lineage
}

func openLineageStore(t *testing.T, path string) *taskdir.Store {
	t.Helper()
	store, err := taskdir.InitStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return store
}

func writeLineageFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (l nativeLineage) gitIn(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	var output, diagnostic bytes.Buffer
	command.Stdout, command.Stderr = &output, &diagnostic
	if err := l.scope.Run(command, execution.CommandHooks{}); err != nil {
		t.Fatalf("native Git %v: %v: %s", args, err, diagnostic.String())
	}
	return strings.TrimSuffix(output.String(), "\n")
}

// gitSucceeds reports whether an owned native Git query exits zero.
func (l nativeLineage) gitSucceeds(directory string, args ...string) bool {
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	return l.scope.Run(command, execution.CommandHooks{}) == nil
}

func (l nativeLineage) gitInput(t *testing.T, directory, input string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	var diagnostic bytes.Buffer
	command.Stdin, command.Stderr = strings.NewReader(input), &diagnostic
	if err := l.scope.Run(command, execution.CommandHooks{}); err != nil {
		t.Fatalf("native Git %v: %v: %s", args, err, diagnostic.String())
	}
}

func (l nativeLineage) commit(t *testing.T, directory, message string) string {
	t.Helper()
	l.gitIn(t, directory, "-c", "user.name=Delegate fixture", "-c", "user.email=fixture@example.invalid", "commit", "--quiet", "--allow-empty", "-m", message)
	return l.gitIn(t, directory, "rev-parse", "HEAD")
}

// linkedSource adds a detached linked worktree, as a parent managed workspace
// would own, at path below the shared container.
func (l nativeLineage) linkedSource(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	l.gitIn(t, l.main, "worktree", "add", "--quiet", "--detach", path, "HEAD")
	return path
}

func (l nativeLineage) resource(t *testing.T, store *taskdir.Store, sourceCwd, container string) *workspace.Resource {
	t.Helper()
	workspaceID, err := randomid.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	return l.resourceWithID(t, store, sourceCwd, container, workspaceID)
}

func (l nativeLineage) resourceWithID(t *testing.T, store *taskdir.Store, sourceCwd, container, workspaceID string) *workspace.Resource {
	t.Helper()
	taskID, err := randomid.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	resource, err := workspace.EstablishResource(l.scope.ctx, store, workspace.ResourceRequest{
		SchemaVersion: task.SchemaVersion, RootID: store.RootID, WorkspaceID: workspaceID, TaskID: taskID,
		RequestSHA256: task.ComputeSHA256([]byte("lineage " + taskID)), SourceCwd: sourceCwd,
		Container: container, BudgetNanos: int64(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := resource.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return resource
}

func (l nativeLineage) provisionedChild(t *testing.T, sourceCwd string) (*workspace.Resource, workspace.Registration) {
	t.Helper()
	resource := l.resource(t, l.store, sourceCwd, l.container)
	if err := provisionManagedCheckout(l.scope.ctx, l.store, resource, l.git); err != nil {
		t.Fatalf("provision sibling child: %v", err)
	}
	registration, err := resource.Registration(l.scope.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return resource, registration
}

func (l nativeLineage) captureOperation(t *testing.T, resource *workspace.Resource, subject workspace.Subject) *workspace.Operation {
	t.Helper()
	operationID, err := randomid.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	digest := task.ComputeSHA256([]byte("capture " + operationID))
	operation, err := workspace.OpenOperation(l.scope.ctx, l.store, workspace.OperationRequest{
		SchemaVersion: task.SchemaVersion, RootID: l.store.RootID, OperationID: operationID, WorkspaceID: resource.Request().WorkspaceID,
		Kind: workspace.OperationCapture, Subject: subject, Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		WorkerExecutable: "/fixture/delegate-run", WorkerSHA256: digest,
		Supervisor: supervisorfixture.ContinuationSupervisorRef(l.store.Root, digest, "unix:/fixture/pueue.sock"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := operation.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return operation
}

func (l nativeLineage) capture(t *testing.T, resource *workspace.Resource, subject workspace.Subject) workspace.CaptureCandidate {
	t.Helper()
	operation := l.captureOperation(t, resource, subject)
	if _, err := captureManagedCheckout(l.scope.ctx, l.store, resource, operation, l.git); err != nil {
		t.Fatalf("capture: %v", err)
	}
	candidate, err := resource.CaptureCandidate(l.scope.ctx, operation.Request().OperationID)
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}

func lineageOperationSubject(label string) workspace.Subject {
	return workspace.Subject{Kind: workspace.SubjectOperation, ID: strings.Repeat("9", 32), SHA256: task.ComputeSHA256([]byte(label))}
}

func (l nativeLineage) ownedRefs(t *testing.T, resource *workspace.Resource) string {
	t.Helper()
	return l.gitIn(t, l.main, "for-each-ref", "--format=%(refname) %(objectname)", "refs/delegate/"+l.store.RootID+"/workspaces/"+resource.Request().WorkspaceID+"/")
}
