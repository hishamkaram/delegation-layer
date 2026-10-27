package tasklifecycle_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
)

func TestHeldCollectionCohortCollectsPendingPublication(t *testing.T) {
	for _, shape := range collectionCohortShapes {
		t.Run(shape.name, func(t *testing.T) {
			cohort := testsupport.NewHeldCollectionCohort(t, 4096, shape.tasks)
			collectors := make([]*testsupport.CollectionHandle, shape.collectors)
			for index := range collectors {
				collectors[index] = cohort.OpenCollector(t, index%shape.tasks)
			}
			if got := len(cohort.TaskIDs()); got != shape.tasks {
				t.Fatalf("task count = %d, want %d", got, shape.tasks)
			}
			completed, err := collectCohortOnce(cohort.Root(), collectors, tasklifecycle.CollectionOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if completed != shape.collectors {
				t.Fatalf("completed = %d, want %d", completed, shape.collectors)
			}
			requireLeasesHeld(t, collectors)
		})
	}
}

// TestRunCohortEntersWorkersTogetherAndJoinsAll proves that the callbacks run
// concurrently and that runCohort joins all of them before it returns. It does
// not prove the start barrier itself: an unsynchronized concurrent runner also
// passes. One shared abort, closed by a hang-guard deadline and not a latency
// bound, releases every callback if they are serialized, so a serialized
// runner fails the assertion below instead of hanging.
func TestRunCohortEntersWorkersTogetherAndJoinsAll(t *testing.T) {
	const workers = 8
	var entered, finished atomic.Int64
	allIn := make(chan struct{})
	abort := make(chan struct{})
	guard := time.AfterFunc(10*time.Second, func() { close(abort) })
	calls := make([]atomic.Int64, workers)
	errs := runCohort(workers, func(index int) error {
		calls[index].Add(1)
		if entered.Add(1) == workers {
			close(allIn)
		}
		select {
		case <-allIn:
		case <-abort:
			return errors.New("worker never saw all workers enter together")
		}
		finished.Add(1)
		return nil
	})
	guard.Stop()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if got := finished.Load(); got != workers {
		t.Fatalf("finished before return = %d, want %d", got, workers)
	}
	for index := range calls {
		if got := calls[index].Load(); got != 1 {
			t.Fatalf("worker %d ran %d times, want 1", index, got)
		}
	}
}

func TestRunCohortReportsErrorsByIndex(t *testing.T) {
	failure := errors.New("worker 2 failed")
	errs := runCohort(4, func(index int) error {
		if index == 2 {
			return failure
		}
		return nil
	})
	for index, err := range errs {
		if (err != nil) != (index == 2) || (err != nil && !errors.Is(err, failure)) {
			t.Fatalf("errs[%d] = %v", index, err)
		}
	}
}

func requireLeasesHeld(t *testing.T, collectors []*testsupport.CollectionHandle) {
	t.Helper()
	for index, collector := range collectors {
		released, err := collector.TaskDir().RunnerLeaseReleased()
		if err != nil || released {
			t.Fatalf("collector %d runner lease released = %t, %v; want held", index, released, err)
		}
	}
}

func TestHeldCollectionCohortHasNoSupervisorAuthority(t *testing.T) {
	cohort := testsupport.NewHeldCollectionCohort(t, 1024, 1)
	handle := cohort.OpenCollector(t, 0)
	if _, err := collectCohortOnce(cohort.Root(), []*testsupport.CollectionHandle{handle}, tasklifecycle.CollectionOptions{Watch: 0}); err != nil {
		t.Fatal(err)
	}
	// Submission is the supervisor admission record; its absence and the
	// absence of the inert supervisor paths show nothing reached a supervisor.
	for _, path := range []string{
		filepath.Join(handle.TaskDir().Dir, "submit.json"),
		filepath.Join(handle.TaskDir().Dir, "supervisor.ref.json"),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s exists or failed: %v", path, err)
		}
	}
	_, meta, err := handle.TaskDir().PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(meta.SupervisorConfig.ConfigPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("supervisor config path exists or failed: %v", err)
	}
}

func TestHeldCollectionCohortBriefIsExactAndCollectorsAreIndependent(t *testing.T) {
	const size = 70000
	cohort := testsupport.NewHeldCollectionCohort(t, size, 2)
	first, second := cohort.OpenCollector(t, 0), cohort.OpenCollector(t, 0)
	if first.TaskDir() == second.TaskDir() {
		t.Fatal("collectors share one TaskDir handle")
	}
	brief, err := first.TaskDir().ReadBrief()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(brief, testsupport.FixtureBrief(size)) {
		t.Fatalf("brief is %d bytes and differs from the fixture brief", len(brief))
	}
	other := cohort.OpenCollector(t, 1)
	if other.TaskDir().TaskID == first.TaskDir().TaskID {
		t.Fatal("independent tasks share an identity")
	}
}

func TestHeldCollectionCohortCloseReleasesLeasesAndIsIdempotent(t *testing.T) {
	cohort := testsupport.NewHeldCollectionCohort(t, 1024, 2)
	handles := []*testsupport.CollectionHandle{cohort.OpenCollector(t, 0), cohort.OpenCollector(t, 1)}
	for _, handle := range handles {
		released, err := handle.TaskDir().RunnerLeaseReleased()
		if err != nil || released {
			t.Fatalf("lease released before Close = %t, %v", released, err)
		}
	}
	if err := cohort.Close(); err != nil {
		t.Fatal(err)
	}
	for _, handle := range handles {
		released, err := handle.TaskDir().RunnerLeaseReleased()
		if err != nil || !released {
			t.Fatalf("lease released after Close = %t, %v", released, err)
		}
	}
	if err := cohort.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	for _, handle := range handles {
		if err := handle.Close(); err != nil {
			t.Fatal(err)
		}
		if err := handle.Close(); err != nil {
			t.Fatalf("second handle Close = %v", err)
		}
	}
}
