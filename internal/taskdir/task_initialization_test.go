package taskdir

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const taskInitializationTestTimeout = 5 * time.Second

type initializationCreateResult struct {
	taskDir *TaskDir
	err     error
}

type initializationLockBarrier struct {
	BaseFaultInjector

	target string
	err    error

	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func newInitializationLockBarrier(target string) *initializationLockBarrier {
	return &initializationLockBarrier{
		target:  target,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (b *initializationLockBarrier) OnLockFileBarrier(path string) error {
	if filepath.Base(path) != b.target {
		return nil
	}
	b.enteredOnce.Do(func() { close(b.entered) })
	<-b.release
	return b.err
}

func (b *initializationLockBarrier) OnLockParentDirBarrier(string) error {
	return nil
}

func (b *initializationLockBarrier) OnFlock(string, int) error {
	return nil
}

func (b *initializationLockBarrier) unblock() {
	b.releaseOnce.Do(func() { close(b.release) })
}

type preparedRecordSnapshot struct {
	brief []byte
	task  []byte
	meta  []byte
}

func startInitializationCreate(store *Store, req *task.TaskRecord, meta *task.MetaRecord, brief []byte) <-chan initializationCreateResult {
	done := make(chan initializationCreateResult, 1)
	go func() {
		td, err := store.CreateTask(req.TaskID, req, brief, meta)
		done <- initializationCreateResult{taskDir: td, err: err}
	}()
	return done
}

func cloneInitializationInput(req *task.TaskRecord, meta *task.MetaRecord, brief []byte) (*task.TaskRecord, *task.MetaRecord, []byte) {
	reqCopy := *req
	metaCopy := *meta
	return &reqCopy, &metaCopy, append([]byte(nil), brief...)
}

func awaitInitializationBarrier(t *testing.T, barrier *initializationLockBarrier) {
	t.Helper()
	select {
	case <-barrier.entered:
	case <-time.After(taskInitializationTestTimeout):
		barrier.unblock()
		t.Fatal("creator did not reach the requested lock-file initialization barrier")
	}
}

func receiveInitializationCreate(done <-chan initializationCreateResult) (initializationCreateResult, bool) {
	select {
	case result := <-done:
		return result, true
	case <-time.After(taskInitializationTestTimeout):
		return initializationCreateResult{}, false
	}
}

func snapshotPreparedRecords(t *testing.T, td *TaskDir) preparedRecordSnapshot {
	t.Helper()
	if td == nil {
		t.Fatal("successful task creation returned a nil task directory")
	}
	if _, _, err := td.PreparedRecords(); err != nil {
		t.Fatalf("validating prepared records: %v", err)
	}
	brief, err := td.store.readBytes(filepath.Join(td.Dir, "brief.md"), task.MaxBriefSize)
	if err != nil {
		t.Fatalf("reading prepared brief: %v", err)
	}
	taskBytes, err := td.store.readBytes(filepath.Join(td.Dir, "task.json"), task.MaxControlRecordSize)
	if err != nil {
		t.Fatalf("reading prepared task record: %v", err)
	}
	meta, err := td.store.readBytes(filepath.Join(td.Dir, "meta.json"), task.MaxControlRecordSize)
	if err != nil {
		t.Fatalf("reading prepared metadata: %v", err)
	}
	return preparedRecordSnapshot{
		brief: append([]byte(nil), brief...),
		task:  append([]byte(nil), taskBytes...),
		meta:  append([]byte(nil), meta...),
	}
}

func equalPreparedRecordSnapshots(left, right preparedRecordSnapshot) bool {
	return bytes.Equal(left.brief, right.brief) &&
		bytes.Equal(left.task, right.task) &&
		bytes.Equal(left.meta, right.meta)
}

func validateInitializationCreateResult(t *testing.T, label string, result initializationCreateResult, reference *preparedRecordSnapshot) (*preparedRecordSnapshot, bool) {
	t.Helper()
	if result.err != nil {
		if result.taskDir != nil {
			t.Fatalf("%s returned an error and a task directory: %v", label, result.err)
		}
		if errors.Is(result.err, os.ErrNotExist) {
			t.Fatalf("%s exposed an initialization ENOENT: %v", label, result.err)
		}
		if !errors.Is(result.err, task.ErrLockBusy) {
			t.Fatalf("%s returned an unexpected error: %v", label, result.err)
		}
		return reference, false
	}
	snapshot := snapshotPreparedRecords(t, result.taskDir)
	if reference != nil && !equalPreparedRecordSnapshots(*reference, snapshot) {
		t.Fatalf("%s wrote prepared records different from the concurrent creator", label)
	}
	return &snapshot, true
}

func openInitializationStores(t *testing.T, root string) (*Store, *Store) {
	t.Helper()
	first, err := OpenStore(root)
	if err != nil {
		t.Fatalf("opening first concurrent store: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := first.Close(); closeErr != nil {
			t.Errorf("closing first concurrent store: %v", closeErr)
		}
	})
	second, err := OpenStore(root)
	if err != nil {
		t.Fatalf("opening second concurrent store: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := second.Close(); closeErr != nil {
			t.Errorf("closing second concurrent store: %v", closeErr)
		}
	})
	return first, second
}

func runConcurrentCreateDuringLockInitialization(t *testing.T, target string) {
	t.Helper()
	store := testStore(t)
	first, second := openInitializationStores(t, store.Root)

	req, meta, brief := preparedInput(t, store)
	barrier := newInitializationLockBarrier(target)
	first.SetFaultInjector(barrier)
	var firstDone, secondDone <-chan initializationCreateResult
	var firstResult, secondResult initializationCreateResult
	var firstReceived, secondReceived bool
	defer func() {
		barrier.unblock()
		if firstDone != nil && !firstReceived {
			firstResult = <-firstDone
			firstReceived = true
		}
		if secondDone != nil && !secondReceived {
			secondResult = <-secondDone
			secondReceived = true
		}
		closeInitializationTask(t, "first", firstResult)
		closeInitializationTask(t, "second", secondResult)
	}()

	firstReq, firstMeta, firstBrief := cloneInitializationInput(req, meta, brief)
	firstDone = startInitializationCreate(first, firstReq, firstMeta, firstBrief)
	awaitInitializationBarrier(t, barrier)

	secondReq, secondMeta, secondBrief := cloneInitializationInput(req, meta, brief)
	secondDone = startInitializationCreate(second, secondReq, secondMeta, secondBrief)
	secondResult, secondReceived = receiveInitializationCreate(secondDone)
	barrier.unblock()
	firstResult, firstReceived = receiveInitializationCreate(firstDone)
	if !secondReceived || !firstReceived {
		t.Fatalf("concurrent creators did not both finish (second=%t, first=%t)", secondReceived, firstReceived)
	}

	var reference *preparedRecordSnapshot
	var successes int
	if snapshot, ok := validateInitializationCreateResult(t, "second", secondResult, reference); ok {
		reference = snapshot
		successes++
	}
	if snapshot, ok := validateInitializationCreateResult(t, "first", firstResult, reference); ok {
		reference = snapshot
		successes++
	}
	if successes == 0 {
		t.Fatal("concurrent creators both failed with lock-busy")
	}

	opened, err := store.OpenTask(req.TaskID)
	if err != nil {
		t.Fatalf("opening the prepared task after concurrent creation: %v", err)
	}
	defer func() {
		if err := opened.Close(); err != nil {
			t.Errorf("closing the prepared task: %v", err)
		}
	}()
	finalSnapshot := snapshotPreparedRecords(t, opened)
	if reference == nil || !equalPreparedRecordSnapshots(*reference, finalSnapshot) {
		t.Fatal("final prepared records do not match the successful concurrent creator")
	}
}

func closeInitializationTask(t *testing.T, label string, result initializationCreateResult) {
	t.Helper()
	if result.taskDir == nil {
		return
	}
	if err := result.taskDir.Close(); err != nil {
		t.Errorf("closing %s created task: %v", label, err)
	}
}

func TestConcurrentCreateTaskDuringAdmissionLockInitialization(t *testing.T) {
	runConcurrentCreateDuringLockInitialization(t, ".admission.lock")
}

func TestConcurrentCreateTaskDuringRunLockInitialization(t *testing.T) {
	runConcurrentCreateDuringLockInitialization(t, ".run.lock")
}

var _ LockFaultInjector = (*initializationLockBarrier)(nil)

func TestCreateTaskPreservesInitializationErrorContext(t *testing.T) {
	for _, injected := range []error{os.ErrNotExist, task.ErrLockBusy} {
		t.Run(injected.Error(), func(t *testing.T) {
			store := testStore(t)
			req, meta, brief := preparedInput(t, store)
			barrier := newInitializationLockBarrier(".admission.lock")
			barrier.err = injected
			barrier.unblock()
			store.SetFaultInjector(barrier)
			td, err := store.CreateTask(req.TaskID, req, brief, meta)
			if td != nil {
				closeInitializationTask(t, "unexpected", initializationCreateResult{taskDir: td})
				t.Fatal("failed initialization returned a task handle")
			}
			if !errors.Is(err, injected) {
				t.Fatalf("error identity lost: %v", err)
			}
			if errors.Is(injected, os.ErrNotExist) {
				if !strings.Contains(err.Error(), "opening task locks") {
					t.Fatalf("missing initialization stage: %v", err)
				}
			} else if err.Error() != injected.Error() {
				t.Fatalf("non-ENOENT message changed: %v", err)
			}
		})
	}
}
