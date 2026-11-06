package workspaces

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

type installedOperationRuntime struct {
	unavailableOperationRuntime
	runner string
}

func (r installedOperationRuntime) ManagedRunner() (string, error) { return r.runner, nil }

func TestWorkspaceOperationWriterRecordsInstalledPathWithoutProgramDigest(t *testing.T) {
	store := supervisorfixture.NewInspectionStore(t)
	runner := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	supervisorfixture.WriteExecutable(t, runner, "installed workspace worker")
	binding := supervisorfixture.ContinuationSupervisorRef(store.Root, strings.Repeat("a", 64), "unix:/fixture/pueue.sock")
	data, err := task.MarshalCanonical(struct {
		SchemaVersion int                `json:"schema_version"`
		Binding       task.SupervisorRef `json:"binding"`
	}{task.SchemaVersion, binding})
	if err != nil {
		t.Fatal(err)
	}
	// Canonical historical data only. No daemon or generation authority exists.
	err = pueue.WithBootstrapLockContext(t.Context(), store.Root, func(context.Context) error {
		return os.WriteFile(filepath.Join(store.Root, ".supervisor", "supervisor.identity.json"), data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	subject := workspace.Subject{Kind: workspace.SubjectTask, ID: strings.Repeat("b", 32), SHA256: strings.Repeat("c", 64)}
	request, err := newWorkspaceOperationRequest(t.Context(), store, OperationOptions{Runtime: installedOperationRuntime{runner: runner}}, strings.Repeat("d", 32), OperationSpec{Kind: workspace.OperationCapture, Subject: subject, Budget: time.Minute, Deadline: deadline})
	if err != nil {
		t.Fatal(err)
	}
	if request.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 || request.WorkerSHA256 != "" || request.WorkerExecutable != runner || request.Supervisor != binding || request.Subject != subject || request.Deadline != deadline {
		t.Fatalf("operation writer lost selected worker or canonical input: %+v", request)
	}
}

func TestWorkspaceOperationSelectedWorkerDoesNotRetargetAcceptedPath(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	runner := filepath.Join(root, "installed-worker")
	supervisorfixture.WriteExecutable(t, runner, "workspace worker")
	if err := validateWorkspaceOperationExecutable(root, runner); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "other-worker")
	if err := os.Symlink(runner, alias); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkspaceOperationExecutable(root, alias); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("accepted path was silently retargeted: %v", err)
	}
	legacy := filepath.Join(root, ".supervisor", "delegate-run-"+strings.Repeat("a", 64))
	if err := validateWorkspaceOperationExecutable(root, legacy); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("historical copied executable became a new command: %v", err)
	}
	if err := os.Remove(runner); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkspaceOperationExecutable(root, runner); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing accepted installation did not retain its cause: %v", err)
	}
}
