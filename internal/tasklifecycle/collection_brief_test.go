package tasklifecycle_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

const collectionBriefFixtureSize = 4096

// briefMutations alter the brief file of a held-pending task. Each keeps the
// file a private regular file, so only its content identity differs.
var briefMutations = []struct {
	name   string
	mutate func(brief []byte) []byte
}{
	{"truncated", func(brief []byte) []byte { return brief[:len(brief)-1] }},
	{"emptied", func(brief []byte) []byte { return nil }},
	{"grown", func(brief []byte) []byte { return append(bytes.Clone(brief), 'x') }},
	{"same-length", func(brief []byte) []byte {
		changed := bytes.Clone(brief)
		changed[len(changed)/2] ^= 0xff
		return changed
	}},
}

func writeBriefFile(t *testing.T, path string, brief []byte) {
	t.Helper()
	if err := os.WriteFile(path, brief, 0o600); err != nil {
		t.Fatal(err)
	}
}

// requireConcreteBriefFault asserts that a collection ended on the concrete
// brief identity fault and not on a stale or cached pending publication.
func requireConcreteBriefFault(t *testing.T, result tasklifecycle.CollectionResult, err error) {
	t.Helper()
	if !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("collect error = %v, want %v", err, task.ErrIdentityMismatch)
	}
	if tasklifecycle.IsPureCollectionPendingError(err) {
		t.Fatalf("brief fault classified as pending: %v", err)
	}
	if result.Pending != "" || result.Waiting || result.Completion != nil {
		t.Fatalf("collect reported pending=%q waiting=%t completion=%v beside the brief fault", result.Pending, result.Waiting, result.Completion)
	}
}

// TestCollectObservesBriefChangeOnTheSameHandleBetweenCalls proves that a
// handle that already collected a valid pending task re-validates the brief
// on every call. A cached valid result or stale pending state would return
// the pending publication after the brief changed or ignore its restoration.
func TestCollectObservesBriefChangeOnTheSameHandleBetweenCalls(t *testing.T) {
	for _, mutation := range briefMutations {
		t.Run(mutation.name, func(t *testing.T) {
			cohort := testsupport.NewHeldCollectionCohort(t, collectionBriefFixtureSize, 1)
			handle := cohort.OpenCollector(t, 0)
			td := handle.TaskDir()
			briefPath := filepath.Join(td.Dir, "brief.md")
			original := testsupport.FixtureBrief(collectionBriefFixtureSize)

			result, err := tasklifecycle.Collect(cohort.Root(), td, tasklifecycle.CollectionOptions{})
			if pendingErr := pendingPublicationError(result, err); pendingErr != nil {
				t.Fatalf("collect of the intact brief: %v", pendingErr)
			}
			writeBriefFile(t, briefPath, mutation.mutate(original))
			result, err = tasklifecycle.Collect(cohort.Root(), td, tasklifecycle.CollectionOptions{})
			requireConcreteBriefFault(t, result, err)

			writeBriefFile(t, briefPath, original)
			result, err = tasklifecycle.Collect(cohort.Root(), td, tasklifecycle.CollectionOptions{})
			if pendingErr := pendingPublicationError(result, err); pendingErr != nil {
				t.Fatalf("collect after the brief was restored: %v", pendingErr)
			}
		})
	}
}

// TestCollectWatchObservesBriefChangeBetweenPasses changes the brief while a
// watch is already past its first pass. The watch must end on the concrete
// fault, before its bound, instead of reporting a stale pending publication.
func TestCollectWatchObservesBriefChangeBetweenPasses(t *testing.T) {
	for _, mutation := range briefMutations {
		t.Run(mutation.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cohort := testsupport.NewHeldCollectionCohort(t, collectionBriefFixtureSize, 1)
				handle := cohort.OpenCollector(t, 0)
				td := handle.TaskDir()
				briefPath := filepath.Join(td.Dir, "brief.md")
				const watch = 300 * time.Millisecond
				const changeAfter = 100 * time.Millisecond

				changed := make(chan error, 1)
				go func() {
					time.Sleep(changeAfter)
					changed <- os.WriteFile(briefPath, mutation.mutate(testsupport.FixtureBrief(collectionBriefFixtureSize)), 0o600)
				}()
				start := time.Now()
				result, err := tasklifecycle.Collect(cohort.Root(), td, tasklifecycle.CollectionOptions{Watch: watch})
				elapsed := time.Since(start)
				if writeErr := <-changed; writeErr != nil {
					t.Fatal(writeErr)
				}
				requireConcreteBriefFault(t, result, err)
				if elapsed < changeAfter || elapsed >= watch {
					t.Fatalf("watch ended after %s, want a fault after the change at %s and before %s", elapsed, changeAfter, watch)
				}
			})
		})
	}
}
