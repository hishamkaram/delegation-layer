package app

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	supervisorfixture "github.com/hishamkaram/delegation-layer/internal/supervisor/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/workspace"
)

const baselineScratchPrefix = "delegate-turn-baseline-"

// scratchWitness records, while each Git command runs, which temporary parents
// hold the baseline's invocation scratch.
type scratchWitness struct {
	runner     workspace.Git
	selected   string
	ambient    string
	inSelected int
	inAmbient  int
}

func (w *scratchWitness) Run(ctx context.Context, command *exec.Cmd) error {
	w.inSelected += countBaselineScratch(w.selected)
	w.inAmbient += countBaselineScratch(w.ambient)
	return w.runner.Runner.Run(ctx, command)
}

func countBaselineScratch(parent string) int {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return -1
	}
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), baselineScratchPrefix) {
			count++
		}
	}
	return count
}

func TestManagedTurnBaselineScratchUsesSelectedTemporaryRoot(t *testing.T) {
	lineage := newNativeLineage(t)
	resource, registration := lineage.provisionedChild(t, lineage.main)
	turn := lineage.beginTurn(t, resource, registration)
	selected, ambient := supervisorfixture.CanonicalTempDir(t), supervisorfixture.CanonicalTempDir(t)
	t.Setenv("TMPDIR", ambient)
	environment := append(slices.DeleteFunc(os.Environ(), func(entry string) bool { return strings.HasPrefix(entry, "TMPDIR=") }), "TMPDIR="+selected)
	witness := &scratchWitness{runner: lineage.git, selected: selected, ambient: ambient}
	git := workspace.Git{Runner: witness, Environment: environment}

	if err := recordManagedTurnBaseline(lineage.scope.ctx, lineage.store, turn.request, git); err != nil {
		t.Fatalf("baseline with a selected temporary root: %v", err)
	}
	if witness.inSelected == 0 {
		t.Fatal("baseline scratch was never observed under the selected temporary root")
	}
	if witness.inAmbient != 0 {
		t.Fatalf("baseline scratch appeared under the process temporary root %d times", witness.inAmbient)
	}
	for _, parent := range []string{selected, ambient} {
		if count := countBaselineScratch(parent); count != 0 {
			t.Fatalf("baseline scratch left %d entries under %q", count, parent)
		}
	}
}
