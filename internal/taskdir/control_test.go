package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type controlTestRecord struct {
	Request string `json:"request"`
}

func TestInspectionControlIsCreateOnceAndOutsideTasks(t *testing.T) {
	s := testStore(t)
	id := strings.Repeat("a", 32)
	d, err := s.OpenInspection(id, true)
	must(t, err)
	t.Cleanup(func() { must(t, d.Close()) })
	record := controlTestRecord{Request: "request-digest"}
	created, err := d.Put("submission.json", record)
	must(t, err)
	if !created {
		t.Fatal("first guard was not created")
	}
	created, err = d.Put("submission.json", record)
	must(t, err)
	if created {
		t.Fatal("identical replay obtained another submission guard")
	}
	created, err = d.Put("submission.json", controlTestRecord{Request: "changed-digest"})
	if created || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("conflicting winner was accepted: created=%v err=%v", created, err)
	}
	var got controlTestRecord
	must(t, d.Read("submission.json", &got))
	if got != record {
		t.Fatal("winner changed")
	}
	if _, err = s.OpenTask(id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control operation admitted an ordinary task: %v", err)
	}
}

func TestInspectionControlRejectsTraversalAndSymlinks(t *testing.T) {
	s := testStore(t)
	if _, err := s.OpenInspection("../tasks", true); err == nil {
		t.Fatal("unsafe operation ID accepted")
	}
	d, err := s.OpenInspection(strings.Repeat("a", 32), true)
	must(t, err)
	t.Cleanup(func() { must(t, d.Close()) })
	for _, name := range []string{"../request.json", ".operation.lock", ".json", "request/data.json"} {
		if _, err = d.Put(name, controlTestRecord{}); err == nil {
			t.Fatalf("unsafe record name accepted: %s", name)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	must(t, os.WriteFile(outside, []byte("{\"request\":\"outside\"}\n"), 0o600))
	must(t, os.Symlink(outside, filepath.Join(d.dir, "result.json")))
	var got controlTestRecord
	if err = d.Read("result.json", &got); err == nil {
		t.Fatal("symlink record read escaped the journal")
	}
	if _, err = d.Put("result.json", controlTestRecord{Request: "replacement"}); err == nil {
		t.Fatal("symlink record was accepted as an immutable winner")
	}
}

func TestInspectionControlConcurrentClaimsHaveOneWinner(t *testing.T) {
	s := testStore(t)
	id := strings.Repeat("b", 32)
	var dirs [2]*ControlDir
	for i := range dirs {
		var err error
		dirs[i], err = s.OpenInspection(id, true)
		must(t, err)
		t.Cleanup(func() { must(t, dirs[i].Close()) })
	}
	var wg sync.WaitGroup
	created := make([]bool, len(dirs))
	errs := make([]error, len(dirs))
	for i, d := range dirs {
		wg.Go(func() {
			errs[i] = d.WithLock(context.Background(), func(tx *ControlTransaction) error {
				var err error
				created[i], err = tx.Put("submission.json", controlTestRecord{Request: "one"})
				return err
			})
		})
	}
	wg.Wait()
	for _, err := range errs {
		must(t, err)
	}
	if created[0] == created[1] {
		t.Fatalf("wanted exactly one guard winner: %v", created)
	}
}

func TestInspectionControlUncertainLinkCannotGrantReplay(t *testing.T) {
	s := testStore(t)
	d, err := s.OpenInspectionGroup(true)
	must(t, err)
	t.Cleanup(func() { must(t, d.Close()) })
	s.SetFaultInjector(&orderedFault{target: "request.json", fail: "post-link"})
	created, err := d.Put("request.json", controlTestRecord{Request: "group-binding"})
	if created || !errors.Is(err, task.ErrUncertainDurability) {
		t.Fatalf("uncertain commit granted authority: created=%v err=%v", created, err)
	}
	s.SetFaultInjector(nil)
	created, err = d.Put("request.json", controlTestRecord{Request: "group-binding"})
	must(t, err)
	if created {
		t.Fatal("replay granted a second mutation after an uncertain commit")
	}
}

func TestInspectionControlClosedHandleRefusesWithoutWriting(t *testing.T) {
	s := testStore(t)
	d, err := s.OpenInspection(strings.Repeat("c", 32), true)
	must(t, err)
	t.Cleanup(func() { must(t, d.Close()) })
	must(t, d.Close())

	created, err := d.Put("submission.json", controlTestRecord{Request: "closed"})
	if created || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed control accepted Put: created=%v err=%v", created, err)
	}
	var got controlTestRecord
	if err = d.Read("submission.json", &got); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed control accepted Read: %v", err)
	}
	if err = d.WithLock(context.Background(), func(_ *ControlTransaction) error {
		t.Fatal("closed control ran a transaction")
		return nil
	}); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed control accepted WithLock: %v", err)
	}
	testAbsent(t, filepath.Join(d.dir, "submission.json"))
}

func TestInspectionControlCloseWaitsForActiveTransaction(t *testing.T) {
	s := testStore(t)
	d, err := s.OpenInspection(strings.Repeat("d", 32), true)
	must(t, err)
	t.Cleanup(func() { must(t, d.Close()) })

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseTransaction := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseTransaction()

	transactionDone := make(chan error, 1)
	go func() {
		transactionDone <- d.WithLock(context.Background(), func(tx *ControlTransaction) error {
			close(entered)
			<-release
			created, putErr := tx.Put("submission.json", controlTestRecord{Request: "transaction"})
			if putErr != nil {
				return putErr
			}
			if !created {
				return errors.New("transaction Put did not create its record")
			}
			return nil
		})
	}()
	<-entered

	closeDone := make(chan error, 1)
	go func() { closeDone <- d.Close() }()
	select {
	case err = <-closeDone:
		t.Fatalf("Close invalidated active transaction: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	releaseTransaction()
	must(t, <-transactionDone)
	must(t, <-closeDone)

	created, err := d.Put("after-close.json", controlTestRecord{Request: "closed"})
	if created || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed control accepted post-transaction Put: created=%v err=%v", created, err)
	}
}

func TestInspectionControlDrainsTransactionOperationsBeforeUnlock(t *testing.T) {
	s := testStore(t)
	d, err := s.OpenInspection(strings.Repeat("f", 32), true)
	must(t, err)
	d2, err := s.OpenInspection(strings.Repeat("f", 32), true)
	must(t, err)
	t.Cleanup(func() { must(t, d.Close()) })
	t.Cleanup(func() { must(t, d2.Close()) })

	started := make(chan struct{})
	release := make(chan struct{})
	fault := &blockingLinkFault{started: started, release: release}
	s.SetFaultInjector(fault)
	defer func() {
		s.SetFaultInjector(nil)
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	var retained *ControlTransaction
	putDone := make(chan error, 1)
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- d.WithLock(context.Background(), func(tx *ControlTransaction) error {
			retained = tx
			go func() {
				_, putErr := tx.Put("submission.json", controlTestRecord{Request: "blocked"})
				putDone <- putErr
			}()
			<-started
			return nil
		})
	}()
	<-started

	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- d2.WithLock(context.Background(), func(_ *ControlTransaction) error {
			close(secondEntered)
			return nil
		})
	}()
	select {
	case <-secondEntered:
		t.Fatal("second transaction entered while first transaction Put was blocked")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	must(t, <-firstDone)
	must(t, <-putDone)
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("second transaction did not enter after first operation drained")
	}
	must(t, <-secondDone)
	if retained == nil {
		t.Fatal("transaction was not retained for the scope check")
	}
	if _, err = retained.Put("after.json", controlTestRecord{Request: "late"}); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("retained transaction remained usable: %v", err)
	}
}

func TestInspectionControlReplayCleansOnlyOwnedStage(t *testing.T) {
	s := testStore(t)
	d, err := s.OpenInspectionGroup(true)
	must(t, err)
	t.Cleanup(func() { must(t, d.Close()) })
	sentinel := filepath.Join(d.dir, "stage."+strings.Repeat("e", 32)+".tmp")
	writeTestFile(t, sentinel, []byte("unrelated stage"))

	created, err := d.Put("request.json", controlTestRecord{Request: "replay"})
	if !created {
		t.Fatalf("initial control record was not created: %v", err)
	}

	fault := &controlCleanupFault{fail: true}
	s.SetFaultInjector(fault)
	created, err = d.Put("request.json", controlTestRecord{Request: "replay"})
	if created || err == nil {
		t.Fatalf("cleanup failure was hidden: created=%v err=%v", created, err)
	}
	if len(fault.stages) != 1 {
		t.Fatalf("cleanup hook count=%d, want one owned stage", len(fault.stages))
	}
	if fault.stages[0] == sentinel {
		t.Fatal("replay cleanup targeted an unrelated stage")
	}
	if got := string(readTestFile(t, sentinel)); got != "unrelated stage" {
		t.Fatalf("unrelated stage changed: %q", got)
	}
	if _, err = os.Stat(fault.stages[0]); err != nil {
		t.Fatalf("owned stage was removed despite cleanup fault: %v", err)
	}

	s.SetFaultInjector(nil)
	created, err = d.Put("request.json", controlTestRecord{Request: "replay"})
	if created || err != nil {
		t.Fatalf("replay after cleanup recovery failed: created=%v err=%v", created, err)
	}
	if got := string(readTestFile(t, sentinel)); got != "unrelated stage" {
		t.Fatalf("unrelated stage changed after recovery: %q", got)
	}
	entries, err := os.ReadDir(d.dir)
	must(t, err)
	stageCount := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "stage.") && strings.HasSuffix(entry.Name(), ".tmp") {
			stageCount++
		}
	}
	if stageCount != 2 {
		t.Fatalf("stage count=%d after replay recovery, want sentinel plus failed owned stage", stageCount)
	}
}

type controlCleanupFault struct {
	BaseFaultInjector
	fail   bool
	stages []string
}

func (f *controlCleanupFault) OnCleanupUnlink(stage string) error {
	f.stages = append(f.stages, stage)
	if f.fail {
		return errors.New("injected cleanup failure")
	}
	return nil
}

type blockingLinkFault struct {
	BaseFaultInjector
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (f *blockingLinkFault) OnLink(_, dest string) error {
	if filepath.Base(dest) != "submission.json" {
		return nil
	}
	f.once.Do(func() { close(f.started) })
	<-f.release
	return nil
}

func TestControlPutRetainsCallerCancellation(t *testing.T) {
	s := testStore(t)
	d, err := s.OpenInspection(strings.Repeat("c", 32), true)
	must(t, err)
	t.Cleanup(func() { must(t, d.Close()) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	created, err := d.PutContext(ctx, "cancelled.json", controlTestRecord{Request: "cancelled"})
	if created || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write acquired authority: created=%v err=%v", created, err)
	}
	var record controlTestRecord
	if err = d.Read("cancelled.json", &record); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled write left a record: %v", err)
	}
}

func TestControlTransactionPutRetainsCallerCancellation(t *testing.T) {
	s := testStore(t)
	d, err := s.OpenInspection(strings.Repeat("d", 32), true)
	must(t, err)
	t.Cleanup(func() { must(t, d.Close()) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = d.WithLock(ctx, func(tx *ControlTransaction) error {
		cancel()
		created, putErr := tx.Put("cancelled.json", controlTestRecord{Request: "cancelled"})
		if created {
			t.Error("cancelled transaction acquired authority")
		}
		return putErr
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("transaction lost caller cancellation: %v", err)
	}
}

type cancelBeforeControlStage struct {
	BaseFaultInjector
	cancel context.CancelFunc
	stage  string
}

func (f *cancelBeforeControlStage) OnBeforeStageCreate(stage, _ string) error {
	f.stage = stage
	f.cancel()
	return nil
}

func TestControlCancellationBeforeStageDoesNotConsumeGuard(t *testing.T) {
	s := testStore(t)
	d, err := s.OpenInspection(strings.Repeat("e", 32), true)
	must(t, err)
	t.Cleanup(func() { must(t, d.Close()) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hook := &cancelBeforeControlStage{cancel: cancel}
	s.SetFaultInjector(hook)
	created, err := d.PutContext(ctx, "submission.json", controlTestRecord{Request: "request"})
	if created || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled staging consumed guard: created=%v err=%v", created, err)
	}
	if hook.stage == "" {
		t.Fatal("cancellation did not reach staging boundary")
	}
	if _, err = os.Lstat(hook.stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled staging left a stage: %v", err)
	}
	var record controlTestRecord
	if err = d.Read("submission.json", &record); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled staging published a guard: %v", err)
	}
	s.SetFaultInjector(nil)
	created, err = d.Put("submission.json", controlTestRecord{Request: "request"})
	must(t, err)
	if !created {
		t.Fatal("cancelled staging consumed the only claim")
	}
}

func TestOpenInspectionRetainsCallerDeadline(t *testing.T) {
	s := testStore(t)
	id := strings.Repeat("f", 32)
	must(t, s.maintLock.LockEX(context.Background()))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	completed := make(chan error, 1)
	go func() {
		d, err := s.OpenInspectionContext(ctx, id, true)
		if d != nil {
			err = errors.Join(err, d.Close())
		}
		completed <- err
	}()
	var got error
	select {
	case got = <-completed:
		must(t, s.maintLock.Unlock())
	case <-time.After(2 * time.Second):
		must(t, s.maintLock.Unlock())
		<-completed
		t.Fatal("inspection open ignored caller deadline while maintenance was held")
	}
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("inspection open lost deadline: %v", got)
	}
	if _, err := os.Lstat(filepath.Join(s.Root, "inspections", id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired open created inspection state: %v", err)
	}
	d, err := s.OpenInspectionContext(context.Background(), id, true)
	must(t, err)
	must(t, d.Close())
}
