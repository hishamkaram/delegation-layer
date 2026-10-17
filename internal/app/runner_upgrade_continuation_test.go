package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/supervisor"
	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestBuildContinuationArgumentsDoesNotGuessRemovedLegacyRunnerIsManaged(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	missing := filepath.Join(root, "removed-install", "delegate-run")
	meta := &task.MetaRecord{RunnerExecutable: missing}
	if _, err := buildContinuationArguments(Arguments{}, root, &task.TaskRecord{}, meta, nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ambiguous removed legacy runner error = %v, want not-exist", err)
	}
	if ownership := supervisor.PersistedRunnerOwnership(root, meta); ownership != task.RunnerOwnershipCustom {
		t.Fatalf("removed legacy runner ownership = %q, want fail-closed custom", ownership)
	}

	writeRunnerFixture(t, missing, "available runner")
	got, err := buildContinuationArguments(Arguments{}, root, &task.TaskRecord{}, meta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Runner != missing {
		t.Fatalf("continuation runner = %q, want saved runner %q", got.Runner, missing)
	}
	if requestedRunnerOwnership(got) != task.RunnerOwnershipCustom {
		t.Fatalf("available legacy custom runner ownership = %q, want custom", requestedRunnerOwnership(got))
	}
}

func TestBuildContinuationArgumentsFailsWhenCustomRunnerWasRemoved(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	missing := filepath.Join(root, "removed-custom-runner", "delegate-run")
	meta := &task.MetaRecord{RunnerExecutable: missing, RunnerOwnership: task.RunnerOwnershipCustom}
	if _, err := buildContinuationArguments(Arguments{}, root, &task.TaskRecord{}, meta, nil); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed custom runner error = %v, want not-exist", err)
	}

	writeRunnerFixture(t, missing, "custom runner")
	got, err := buildContinuationArguments(Arguments{}, root, &task.TaskRecord{}, meta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Runner != missing {
		t.Fatalf("continuation runner = %q, want saved custom runner %q", got.Runner, missing)
	}
	if requestedRunnerOwnership(got) != task.RunnerOwnershipCustom {
		t.Fatalf("continuation ownership = %q, want custom", requestedRunnerOwnership(got))
	}
}

func TestBuildContinuationArgumentsRefreshesStateRootRunner(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	runner := filepath.Join(root, ".supervisor", "delegate-run")
	writeRunnerFixture(t, runner, "old state runner")
	meta := &task.MetaRecord{RunnerExecutable: runner}
	got, err := buildContinuationArguments(Arguments{}, root, &task.TaskRecord{}, meta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Runner != "" {
		t.Fatalf("continuation pinned old state-root runner %q", got.Runner)
	}
	if requestedRunnerOwnership(got) != task.RunnerOwnershipManaged {
		t.Fatalf("state-root continuation ownership = %q, want managed", requestedRunnerOwnership(got))
	}
}

func TestBuildContinuationArgumentsRefreshesContentAddressedRunner(t *testing.T) {
	root := supervisorfixture.CanonicalTempDir(t)
	source := filepath.Join(supervisorfixture.CanonicalTempDir(t), "delegate-run")
	writeRunnerFixture(t, source, "saved runner")
	runner, err := supervisor.InstallStateRunner(root, source)
	if err != nil {
		t.Fatal(err)
	}
	meta := &task.MetaRecord{RunnerExecutable: runner}
	got, err := buildContinuationArguments(Arguments{}, root, &task.TaskRecord{}, meta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Runner != "" {
		t.Fatalf("continuation pinned old content-addressed runner %q", got.Runner)
	}
	if requestedRunnerOwnership(got) != task.RunnerOwnershipManaged {
		t.Fatalf("content-addressed continuation ownership = %q, want managed", requestedRunnerOwnership(got))
	}
}

func writeRunnerFixture(t *testing.T, path, contents string) {
	t.Helper()
	supervisorfixture.WriteExecutable(t, path, contents)
}
