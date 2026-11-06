package execution

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type binderTaskFixture struct {
	td     *taskdir.TaskDir
	permit *taskdir.StartPermit
	plan   Plan
}

// binderTask selects an owned TMPDIR so unexpected executable staging is observable.
func binderTask(t *testing.T) (binderTaskFixture, string) {
	t.Helper()
	temporary, err := filepath.EvalSymlinks(t.TempDir())
	require(t, err)
	td, permit, plan, _ := fixtureTask(t, "echo")
	plan.Environment = append(slices.Clone(plan.Environment), "TMPDIR="+temporary)
	return binderTaskFixture{td: td, permit: permit, plan: plan}, temporary
}

func assertNoTaskViews(t *testing.T, temporary string) {
	t.Helper()
	entries, err := os.ReadDir(temporary)
	require(t, err)
	if len(entries) != 0 {
		t.Fatalf("verified views remain in %s: %v", temporary, entries)
	}
}
