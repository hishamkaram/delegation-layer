package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

type orderedFault struct {
	BaseFaultInjector
	target string
	fail   string
	events []string
}

func (f *orderedFault) record(kind, path string) error {
	name := filepath.Base(path)
	f.events = append(f.events, kind+":"+name)
	if kind == f.fail && (f.target == "" || name == f.target) {
		return errors.New("injected " + kind)
	}
	return nil
}

func (f *orderedFault) OnStageWrite(path string, b []byte) (int, error) {
	if f.fail == "short-write" && filepath.Base(path) == f.target {
		return len(b) - 1, nil
	}
	return len(b), f.record("write", path)
}
func (f *orderedFault) OnStageBarrier(p string) error        { return f.record("file-barrier", p) }
func (f *orderedFault) OnStageClose(p string) error          { return f.record("close", p) }
func (f *orderedFault) OnLink(_, p string) error             { return f.record("link", p) }
func (f *orderedFault) OnPostLinkDirBarrier(p string) error  { return f.record("post-link", p) }
func (f *orderedFault) OnAfterLinkDirBarrier(p string) error { return f.record("after-barrier", p) }
func (f *orderedFault) OnCleanupDestination(dest, _ string) error {
	return f.record("cleanup-destination", dest)
}
func (f *orderedFault) OnCleanupUnlink(p string) error     { return f.record("cleanup", p) }
func (f *orderedFault) OnCleanupDirBarrier(p string) error { return f.record("cleanup-barrier", p) }
func (f *orderedFault) OnDirBarrier(p string) error        { return f.record("directory", p) }
func (f *orderedFault) OnParentDirBarrier(p string) error  { return f.record("parent", p) }
func (f *orderedFault) OnRawWrite(p string) error          { return f.record("raw-write", p) }
func (f *orderedFault) OnRawBarrier(p string) error        { return f.record("raw-barrier", p) }
func (f *orderedFault) OnRawClose(p string) error          { return f.record("raw-close", p) }
func (f *orderedFault) OnRawDirBarrier(p string) error     { return f.record("raw-directory", p) }

func TestOrderedPersistenceFaults(t *testing.T) {
	for _, fault := range []string{"write", "short-write", "file-barrier", "close", "link", "post-link", "after-barrier", "cleanup", "cleanup-barrier"} {
		t.Run(fault, func(t *testing.T) {
			testSubmissionFault(t, fault)
		})
	}
	t.Run("ordered-success", func(t *testing.T) {
		s := testStore(t)
		td := preparedTask(t, s)
		_, meta, err := td.PreparedRecords()
		must(t, err)
		hook := &orderedFault{}
		s.SetFaultInjector(hook)
		permit, err := td.PrepareSubmission(meta.SupervisorConfig)
		must(t, err)
		must(t, permit.Release())
		var observed []string
		for _, event := range hook.events {
			kind := strings.SplitN(event, ":", 2)[0]
			if kind != "cleanup" && kind != "cleanup-barrier" {
				observed = append(observed, kind)
			}
		}
		expected := []string{"write", "file-barrier", "close", "link", "post-link", "after-barrier", "cleanup-destination"}
		if !reflect.DeepEqual(observed, expected) {
			t.Fatalf("bad order %v", hook.events)
		}
	})
}

func TestStartAndSealFaults(t *testing.T) {
	for _, record := range []string{"provider.start", "provider.exit", "provider.started.json"} {
		t.Run(record, func(t *testing.T) {
			testStartSealFault(t, record)
		})
	}
}

func TestRawFailurePreventsSeal(t *testing.T) {
	for _, kind := range []string{"raw-write", "raw-barrier", "raw-close", "raw-directory"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			td := preparedTask(t, s)
			p := consumeStart(t, td)
			defer func() { must(t, p.Release()) }()
			s.SetFaultInjector(&orderedFault{fail: kind})
			writeErr := td.WriteRawFiles("answer", "")
			if kind == "raw-directory" {
				must(t, writeErr)
			} else {
				if writeErr == nil {
					t.Fatal("raw failure hidden")
				}
				s.SetFaultInjector(nil)
			}
			seal, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
			if err == nil || seal != nil {
				t.Fatal("failed capture sealed")
			}
			testAbsent(t, filepath.Join(td.Dir, "provider.exit"))
		})
	}
}

func TestAncestorRetryBarrier(t *testing.T) {
	s := testStore(t)
	req, meta, brief := preparedInput(t, s)
	hook := &orderedFault{target: "tasks", fail: "parent"}
	s.SetFaultInjector(hook)
	td, err := s.CreateTask(req.TaskID, req, brief, meta)
	if err == nil || td != nil {
		t.Fatal("ancestor failure ignored")
	}
	if _, err = os.Stat(filepath.Join(s.Root, "tasks", req.TaskID)); err != nil {
		t.Fatal("test failed before exact child existed")
	}
	hook.fail = ""
	hook.events = nil
	td, err = s.CreateTask(req.TaskID, req, brief, meta)
	must(t, err)
	defer closeTaskQuietly(td)
	p, err := td.PrepareSubmission(meta.SupervisorConfig)
	must(t, err)
	must(t, p.Release())
	parentIndex, submitIndex := -1, -1
	for index, event := range hook.events {
		if event == "parent:tasks" {
			parentIndex = index
		}
		if event == "link:submit.json" {
			submitIndex = index
		}
	}
	if parentIndex < 0 || submitIndex <= parentIndex {
		t.Fatalf("ancestor retry granted authority before barrier: %v", hook.events)
	}
}

func TestPublicationFailureAndRecovery(t *testing.T) {
	for _, target := range []string{"result.txt", "outcome.json"} {
		t.Run(target, func(t *testing.T) {
			s := testStore(t)
			td := sealedTask(t, s, "answer")
			hook := &orderedFault{target: target, fail: "post-link"}
			s.SetFaultInjector(hook)
			out, cleanup, err := td.Collect(task.FixturePredicateRef())
			must(t, cleanup)
			if out != nil || !errors.Is(err, task.ErrUncertainDurability) {
				t.Fatalf("bad uncertainty %v %+v", err, out)
			}
			before := readTestFile(t, filepath.Join(td.Dir, target))
			hook.fail = ""
			hook.events = nil
			collectOK(t, td)
			if string(before) != string(readTestFile(t, filepath.Join(td.Dir, target))) {
				t.Fatal("recovery changed committed bytes")
			}
			if target == "outcome.json" && !containsEvent(hook.events, "post-link:outcome.json") {
				t.Fatal("winner recovery omitted post-winner barrier")
			}
		})
	}
}

func containsEvent(events []string, want string) bool {
	for _, e := range events {
		if e == want {
			return true
		}
	}
	return false
}

type lockFault struct {
	BaseFaultInjector
	path       string
	failFile   bool
	failParent bool
	failures   []error
	calls      int
}

func (f *lockFault) OnLockFileBarrier(path string) error {
	if f.failFile && filepath.Base(path) == f.path {
		return errors.New("lock file barrier")
	}
	return nil
}

func (f *lockFault) OnLockParentDirBarrier(path string) error {
	if f.failParent && filepath.Base(path) == f.path {
		return errors.New("lock parent barrier")
	}
	return nil
}

func (f *lockFault) OnFlock(path string, operation int) error {
	if filepath.Base(path) != f.path || operation == unix.LOCK_UN {
		return nil
	}
	f.calls++
	if len(f.failures) > 0 {
		e := f.failures[0]
		f.failures = f.failures[1:]
		return e
	}
	return nil
}

func TestLockFailuresAndRetry(t *testing.T) {
	for _, kind := range []string{"file", "parent"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			req, meta, brief := preparedInput(t, s)
			s.SetFaultInjector(&lockFault{path: ".admission.lock", failFile: kind == "file", failParent: kind == "parent"})
			td, err := s.CreateTask(req.TaskID, req, brief, meta)
			if td != nil || err == nil {
				t.Fatal("lock initialization failure granted preparation")
			}
			testAbsent(t, filepath.Join(s.Root, "tasks", req.TaskID, "task.json"))
		})
	}
	t.Run("EINTR-busy-and-error", func(t *testing.T) {
		s := testStore(t)
		td := preparedTask(t, s)
		hook := &lockFault{path: ".run.lock", failures: []error{unix.EINTR}}
		s.SetFaultInjector(hook)
		must(t, td.runLock.LockEXNonblocking())
		if hook.calls != 2 {
			t.Fatalf("EINTR calls %d", hook.calls)
		}
		must(t, td.runLock.Unlock())
		hook.failures = []error{unix.EWOULDBLOCK}
		if err := td.runLock.LockEXNonblocking(); !errors.Is(err, task.ErrLockBusy) {
			t.Fatal(err)
		}
		hook.failures = []error{unix.EBADF}
		if err := td.runLock.LockEXNonblocking(); !errors.Is(err, unix.EBADF) {
			t.Fatal(err)
		}
		must(t, td.runLock.LockEXNonblocking())
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		if err := td.runLock.LockEX(ctx); !errors.Is(err, task.ErrLockBusy) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		must(t, td.runLock.Unlock())
	})
}

func TestDestinationCollisionPreservesExistingBytes(t *testing.T) {
	s := testStore(t)
	td := preparedTask(t, s)
	name := "collision.json"
	committed, cleanup, err := s.stageAndCommit(td.Dir, name, []byte("old"), nil)
	must(t, err)
	must(t, cleanup)
	if !committed {
		t.Fatal("missing initial commit")
	}
	committed, cleanup, err = s.stageAndCommit(td.Dir, name, []byte("new"), nil)
	must(t, cleanup)
	if committed || !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	if string(readTestFile(t, filepath.Join(td.Dir, name))) != "old" {
		t.Fatal("collision replaced bytes")
	}
}

func testSubmissionFault(t *testing.T, fault string) {
	s := testStore(t)
	td := preparedTask(t, s)
	_, meta, err := td.PreparedRecords()
	must(t, err)
	hook := &orderedFault{target: "submit.json", fail: fault}
	if strings.HasPrefix(fault, "cleanup") {
		hook.target = ""
	}
	s.SetFaultInjector(hook)
	permit, err := td.PrepareSubmission(meta.SupervisorConfig)
	if err == nil || permit != nil {
		t.Fatalf("failed %s granted permit: %v", fault, err)
	}
	if fault == "post-link" && !errors.Is(err, task.ErrUncertainDurability) {
		t.Fatalf("lost uncertainty %v", err)
	}
	if fault == "write" || fault == "short-write" || fault == "file-barrier" || fault == "close" || fault == "link" {
		testAbsent(t, filepath.Join(td.Dir, "submit.json"))
	}
	s.SetFaultInjector(nil)
}

func testStartSealFault(t *testing.T, record string) {
	s := testStore(t)
	td := preparedTask(t, s)
	if record == "provider.start" {
		s.SetFaultInjector(&orderedFault{target: record, fail: "close"})
		p, err := td.PrepareStart(0)
		if p != nil || err == nil {
			t.Fatal("start close failure granted authority")
		}
		testAbsent(t, filepath.Join(td.Dir, record))
		return
	}
	p := consumeStart(t, td)
	defer func() { must(t, p.Release()) }()
	s.SetFaultInjector(&orderedFault{target: record, fail: "link"})
	if record == "provider.started.json" {
		if err := td.RecordStarted(1); err == nil {
			t.Fatal("diagnostic failure ignored")
		}
	} else {
		must(t, td.WriteRawFiles("answer", ""))
		seal, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
		if seal != nil || err == nil {
			t.Fatal("seal failure hidden")
		}
	}
	testAbsent(t, filepath.Join(td.Dir, record))
	testAbsent(t, filepath.Join(td.Dir, "outcome.json"))
}
