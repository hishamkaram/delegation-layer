package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor/installation"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestRunnerRepairTargetUsesInstalledPathAndRejectsOtherSelections(t *testing.T) {
	path := filepath.Join(supervisorfixture.CanonicalPath(t, t.TempDir()), "delegate-run")
	selectionExecutable(t, path)
	target, err := PlanQueuedRunnerRepairTarget(path)
	if err != nil {
		t.Fatal(err)
	}
	if target.executable != path {
		t.Fatalf("selected path=%q, want installed %q", target.executable, path)
	}
	if err = target.ValidateExecutable(path); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "delegate-run")
	selectionExecutable(t, other)
	if err = target.ValidateExecutable(other); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("different role accepted: %v", err)
	}
	if err = (RunnerRepairTarget{}).ValidateExecutable(path); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("zero target accepted: %v", err)
	}
	if err = os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = target.ValidateExecutable(path); err == nil {
		t.Fatal("unexecutable installed path accepted")
	}
}

func TestManagedRunnerSuccessorRequiresSelectedRoleAndRecordedOwnership(t *testing.T) {
	original := filepath.Join(t.TempDir(), "recorded-runner")
	selected := filepath.Join(supervisorfixture.CanonicalPath(t, t.TempDir()), "delegate-run")
	selectionExecutable(t, selected)
	target, err := PlanQueuedRunnerRepairTarget(selected)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateManagedOrdinaryRunnerExecutable(original, original, task.RunnerOwnershipManaged, RunnerRepairTarget{}); err != nil {
		t.Fatalf("native-owned recorded image rejected: %v", err)
	}
	if err = ValidateManagedOrdinaryRunnerExecutable(original, selected, task.RunnerOwnershipManaged, target); err != nil {
		t.Fatal(err)
	}
	if err = ValidateManagedOrdinaryRunnerExecutable(original, selected, task.RunnerOwnershipManaged, RunnerRepairTarget{}); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unselected managed successor accepted: %v", err)
	}
	binding := inspection.Binding{WorkerExecutable: original, RunnerOwnership: task.RunnerOwnershipManaged}
	if err = ValidateProviderRunnerExecutable("", binding, task.RunnerOwnershipManaged, selected, target); err != nil {
		t.Fatal(err)
	}
	binding.RunnerOwnership = task.RunnerOwnershipCustom
	if err = ValidateProviderRunnerExecutable("", binding, task.RunnerOwnershipManaged, selected, target); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("custom worker successor accepted: %v", err)
	}
	binding.RunnerOwnership = task.RunnerOwnershipManaged
	if err = ValidateProviderRunnerExecutable("", binding, task.RunnerOwnershipCustom, selected, target); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("custom task successor accepted: %v", err)
	}
}

func TestManagedRetryPreservesRecordedInstallationUntilAbsent(t *testing.T) {
	root := t.TempDir()
	recorded := filepath.Join(supervisorfixture.CanonicalPath(t, t.TempDir()), "recorded-runner")
	installed := filepath.Join(supervisorfixture.CanonicalPath(t, t.TempDir()), "delegate-run")
	selectionExecutable(t, recorded)
	selectionExecutable(t, installed)
	meta := &task.MetaRecord{RunnerExecutable: recorded, RunnerOwnership: task.RunnerOwnershipManaged}
	got, _, err := RefreshManagedRetryRunnerContext(t.Context(), AdmissionSelection{}, installed, root, nil, meta)
	if err != nil || got.Runner != recorded {
		t.Fatalf("recorded install=%q err=%v", got.Runner, err)
	}
	if err = os.Remove(recorded); err != nil {
		t.Fatal(err)
	}
	got, _, err = RefreshManagedRetryRunnerContext(t.Context(), AdmissionSelection{}, installed, root, nil, meta)
	if err != nil || got.Runner != installed {
		t.Fatalf("absent recorded install fallback=%q err=%v", got.Runner, err)
	}
}

func TestResolveRunnerAcceptsInstalledAliasAndPinsResolvedSelection(t *testing.T) {
	directory := supervisorfixture.CanonicalPath(t, t.TempDir())
	real := filepath.Join(directory, "installed-worker")
	alias := filepath.Join(directory, "delegate-run")
	selectionExecutable(t, real)
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	selected, err := installation.ResolveRunner(alias, "")
	if err != nil || selected != real {
		t.Fatalf("installed alias selection=%q err=%v", selected, err)
	}
	target, err := PlanQueuedRunnerRepairTarget(alias)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(directory, "replacement-worker")
	selectionExecutable(t, other)
	if err = os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(other, alias); err != nil {
		t.Fatal(err)
	}
	if err = target.ValidateExecutable(other); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("accepted target retargeted via alias: %v", err)
	}
	if err = target.ValidateExecutable(real); err != nil {
		t.Fatalf("original selected installed path lost: %v", err)
	}
}
