package taskdir

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestControlTransactionVerifyExistingDoesNotStage(t *testing.T) {
	s, d := controlVerifyDir(t)
	record := controlTestRecord{Request: "group-binding"}
	_, err := d.Put("request-v2.json", record)
	must(t, err)
	path := filepath.Join(d.dir, "request-v2.json")
	before, err := os.Stat(path)
	must(t, err)
	data := readTestFile(t, path)
	fault := &controlVerifyFault{}
	s.SetFaultInjector(fault)

	must(t, d.WithLock(t.Context(), func(tx *ControlTransaction) error {
		return tx.Verify("request-v2.json", record)
	}))
	assertControlVerifyEvents(t, fault, []string{"directory:" + d.dir, "parent:" + filepath.Dir(d.dir)})
	after, err := os.Stat(path)
	must(t, err)
	if !os.SameFile(before, after) || !bytes.Equal(data, readTestFile(t, path)) {
		t.Fatal("verification changed the existing winner")
	}
	entries, err := os.ReadDir(d.dir)
	must(t, err)
	if len(entries) != 2 {
		t.Fatalf("verification left unexpected directory entries: %v", entries)
	}
}

func TestControlTransactionVerifyMissingDoesNotCreate(t *testing.T) {
	for _, missingDirectory := range []bool{false, true} {
		name := "record"
		if missingDirectory {
			name = "directory"
		}
		t.Run(name, func(t *testing.T) {
			s, d := controlVerifyDir(t)
			fault := &controlVerifyFault{}
			s.SetFaultInjector(fault)
			err := d.WithLock(t.Context(), func(tx *ControlTransaction) error {
				if missingDirectory {
					if err := os.RemoveAll(d.dir); err != nil {
						return err
					}
				}
				return tx.Verify("request.json", controlTestRecord{Request: "missing"})
			})
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing %s lost its error identity: %v", name, err)
			}
			assertControlVerifyEvents(t, fault, nil)
			testAbsent(t, filepath.Join(d.dir, "request.json"))
			if missingDirectory {
				testAbsent(t, d.dir)
			}
		})
	}
}

func TestControlTransactionVerifyRejectsNoncanonicalWinner(t *testing.T) {
	record := map[string]int{"a": 1, "b": 2}
	for name, data := range map[string]string{
		"reordered":       "{\"b\":2,\"a\":1}\n",
		"whitespace":      "{\"a\":1, \"b\":2}",
		"missing-newline": "{\"a\":1,\"b\":2}",
		"different":       "{\"a\":1,\"b\":3}\n",
		"oversized":       "{\"a\":1,\"b\":2}\n" + strings.Repeat(" ", task.MaxControlRecordSize),
	} {
		t.Run(name, func(t *testing.T) {
			s, d := controlVerifyDir(t)
			path := filepath.Join(d.dir, "request.json")
			writeTestFile(t, path, []byte(data))
			fault := &controlVerifyFault{}
			s.SetFaultInjector(fault)
			err := d.WithLock(t.Context(), func(tx *ControlTransaction) error {
				return tx.Verify("request.json", record)
			})
			if !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("noncanonical winner accepted: %v", err)
			}
			if name == "oversized" && !errors.Is(err, task.ErrControlRecordTooBig) {
				t.Fatalf("oversized winner lost bounded-read error: %v", err)
			}
			assertControlVerifyEvents(t, fault, nil)

			cleanupErr := errors.New("injected cleanup failure")
			fault.cleanupErr = cleanupErr
			created, err := d.Put("request.json", record)
			if created || !errors.Is(err, cleanupErr) || !errors.Is(err, task.ErrEvidenceFault) {
				t.Fatalf("Put lost comparison or cleanup failure: created=%v err=%v", created, err)
			}
			if string(readTestFile(t, path)) != data {
				t.Fatal("rejected winner changed")
			}
		})
	}
}

func TestControlTransactionVerifyValidatesBeforeReading(t *testing.T) {
	for _, tc := range []struct {
		name   string
		file   string
		record any
		want   error
	}{
		{name: "traversal", file: "../request.json", record: func() {}, want: task.ErrEvidenceFault},
		{name: "lock-name", file: ".operation.lock", record: controlTestRecord{}, want: task.ErrEvidenceFault},
		{name: "oversized-candidate", file: "request.json", record: controlTestRecord{Request: strings.Repeat("x", task.MaxControlRecordSize)}, want: task.ErrControlRecordTooBig},
		{name: "marshal-failure", file: "request.json", record: func() {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, d := controlVerifyDir(t)
			fault := &controlVerifyFault{}
			s.SetFaultInjector(fault)
			err := d.WithLock(t.Context(), func(tx *ControlTransaction) error {
				return tx.Verify(tc.file, tc.record)
			})
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid candidate was read or accepted: %v", err)
			}
			assertControlVerifyEvents(t, fault, nil)
			testAbsent(t, filepath.Join(d.dir, "request.json"))
		})
	}
}

func TestControlTransactionVerifyRejectsSymlink(t *testing.T) {
	s, d := controlVerifyDir(t)
	record := controlTestRecord{Request: "outside"}
	data, err := task.MarshalCanonical(record)
	must(t, err)
	outside := filepath.Join(t.TempDir(), "outside.json")
	writeTestFile(t, outside, data)
	must(t, os.Symlink(outside, filepath.Join(d.dir, "request.json")))
	fault := &controlVerifyFault{}
	s.SetFaultInjector(fault)
	err = d.WithLock(t.Context(), func(tx *ControlTransaction) error {
		return tx.Verify("request.json", record)
	})
	if !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("symlink winner accepted: %v", err)
	}
	assertControlVerifyEvents(t, fault, nil)
}

func TestControlTransactionVerifyPreservesDurabilityFault(t *testing.T) {
	for _, barrier := range []string{"directory", "parent"} {
		t.Run(barrier, func(t *testing.T) {
			s, d := controlVerifyDir(t)
			record := controlTestRecord{Request: "group-binding"}
			_, err := d.Put("request.json", record)
			must(t, err)
			barrierErr := errors.Join(task.ErrUncertainDurability, errors.New("injected directory barrier failure"))
			fault := &controlVerifyFault{failAt: barrier, failure: barrierErr}
			s.SetFaultInjector(fault)
			err = d.WithLock(t.Context(), func(tx *ControlTransaction) error {
				return tx.Verify("request.json", record)
			})
			if !errors.Is(err, barrierErr) || !errors.Is(err, task.ErrUncertainDurability) {
				t.Fatalf("verification hid durability fault: %v", err)
			}
			want := []string{"directory:" + d.dir}
			if barrier == "parent" {
				want = append(want, "parent:"+filepath.Dir(d.dir))
			}
			assertControlVerifyEvents(t, fault, want)

			cleanupErr := errors.New("injected cleanup failure")
			fault.cleanupErr = cleanupErr
			created, err := d.Put("request.json", record)
			if created || !errors.Is(err, cleanupErr) || !errors.Is(err, barrierErr) {
				t.Fatalf("Put lost durability or cleanup failure: created=%v err=%v", created, err)
			}
			s.SetFaultInjector(nil)
			must(t, d.WithLock(t.Context(), func(tx *ControlTransaction) error {
				return tx.Verify("request.json", record)
			}))
		})
	}
}

func TestControlVerificationBarrierFailureIsNotRecordAbsence(t *testing.T) {
	for _, tc := range []struct {
		method  string
		barrier string
	}{
		{method: "verify", barrier: "directory"},
		{method: "verify", barrier: "parent"},
		{method: "replay", barrier: "directory"},
		{method: "replay", barrier: "parent"},
	} {
		t.Run(tc.method+"/"+tc.barrier, func(t *testing.T) {
			s, d := controlVerifyDir(t)
			record := controlTestRecord{Request: "group-binding"}
			created, err := d.Put("request.json", record)
			must(t, err)
			if !created {
				t.Fatal("fixture did not create its record")
			}
			path := filepath.Join(d.dir, "request.json")
			before := readTestFile(t, path)
			fault := &controlVerifyFault{failAt: tc.barrier, failure: os.ErrNotExist}
			s.SetFaultInjector(fault)
			t.Cleanup(func() { s.SetFaultInjector(nil) })

			if tc.method == "verify" {
				err = d.WithLock(t.Context(), func(tx *ControlTransaction) error {
					return tx.Verify("request.json", record)
				})
			} else {
				created, err = d.Put("request.json", record)
				if created {
					t.Fatal("replay granted another record claim")
				}
			}
			if !errors.Is(err, os.ErrNotExist) || !errors.Is(err, task.ErrEvidenceFault) || task.RecordAbsent(err) {
				t.Fatalf("verification barrier failure was treated as absence: %v", err)
			}
			if !bytes.Equal(before, readTestFile(t, path)) {
				t.Fatal("verification changed the existing record")
			}

			s.SetFaultInjector(nil)
			must(t, d.WithLock(t.Context(), func(tx *ControlTransaction) error {
				return tx.Verify("request.json", record)
			}))
		})
	}
}

func TestControlTransactionVerifyContextMatchesRead(t *testing.T) {
	for _, lockName := range []string{"maintenance", "operation"} {
		t.Run(lockName, func(t *testing.T) {
			s, d := controlVerifyDir(t)
			lock := d.lock
			if lockName == "maintenance" {
				lock = s.maintLock
			}
			must(t, lock.LockEX(t.Context()))
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			err := d.WithLock(ctx, func(tx *ControlTransaction) error {
				t.Error("cancelled waiter entered the transaction")
				return tx.Verify("request.json", controlTestRecord{})
			})
			must(t, lock.Unlock())
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("transaction lost caller cancellation: %v", err)
			}
		})
	}
	t.Run("admitted-read", func(t *testing.T) {
		_, d := controlVerifyDir(t)
		record := controlTestRecord{Request: "group-binding"}
		_, err := d.Put("request.json", record)
		must(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		must(t, d.WithLock(ctx, func(tx *ControlTransaction) error {
			cancel()
			var got controlTestRecord
			if err := tx.Read("request.json", &got); err != nil {
				return err
			}
			return tx.Verify("request.json", got)
		}))
	})
}

func TestControlTransactionVerifyRejectsInactiveScope(t *testing.T) {
	_, d := controlVerifyDir(t)
	var retained *ControlTransaction
	must(t, d.WithLock(t.Context(), func(tx *ControlTransaction) error {
		retained = tx
		return nil
	}))
	must(t, d.Close())
	for _, tx := range []*ControlTransaction{nil, {}, retained} {
		if err := tx.Verify("../invalid.json", func() {}); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("inactive transaction did not reject before validation: %v", err)
		}
	}
}

func TestControlTransactionVerifyDrainsBeforeUnlock(t *testing.T) {
	s, d := controlVerifyDir(t)
	d2, err := s.OpenInspectionGroup(false)
	must(t, err)
	t.Cleanup(func() { must(t, d2.Close()) })
	record := controlTestRecord{Request: "group-binding"}
	_, err = d.Put("request.json", record)
	must(t, err)
	started := make(chan struct{})
	release := make(chan struct{})
	s.SetFaultInjector(&controlVerifyBlockingBarrier{started: started, release: release})
	verifyDone := make(chan error, 1)
	transactionDone := make(chan error, 1)
	go func() {
		transactionDone <- d.WithLock(t.Context(), func(tx *ControlTransaction) error {
			go func() { verifyDone <- tx.Verify("request.json", record) }()
			<-started
			return nil
		})
	}()
	defer func() {
		close(release)
		must(t, <-verifyDone)
		must(t, <-transactionDone)
	}()
	<-started
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err = d2.WithLock(ctx, func(_ *ControlTransaction) error {
		t.Error("second transaction entered before verification drained")
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("operation lease was not retained through verification: %v", err)
	}
}

func TestControlTransactionVerifyRemainsValidWhileClosing(t *testing.T) {
	_, d := controlVerifyDir(t)
	record := controlTestRecord{Request: "group-binding"}
	_, err := d.Put("request.json", record)
	must(t, err)
	closeDone := make(chan error, 1)
	err = d.WithLock(t.Context(), func(tx *ControlTransaction) error {
		go func() { closeDone <- d.Close() }()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			d.mu.Lock()
			closed := d.closed
			d.mu.Unlock()
			if closed {
				return tx.Verify("request.json", record)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	})
	must(t, <-closeDone)
	must(t, err)
}

func controlVerifyDir(t *testing.T) (*Store, *ControlDir) {
	t.Helper()
	s := testStore(t)
	d, err := s.OpenInspectionGroup(true)
	must(t, err)
	t.Cleanup(func() { must(t, d.Close()) })
	return s, d
}

func assertControlVerifyEvents(t *testing.T, fault *controlVerifyFault, want []string) {
	t.Helper()
	if fault.stages != 0 || !slices.Equal(fault.barriers, want) {
		t.Fatalf("verification persistence events: stages=%d barriers=%v; want stages=0 barriers=%v", fault.stages, fault.barriers, want)
	}
}

type controlVerifyFault struct {
	BaseFaultInjector
	stages     int
	barriers   []string
	failAt     string
	failure    error
	cleanupErr error
}

func (f *controlVerifyFault) OnBeforeStageCreate(_, _ string) error {
	f.stages++
	return nil
}

func (f *controlVerifyFault) OnDirBarrier(path string) error {
	return f.barrier("directory", path)
}

func (f *controlVerifyFault) OnParentDirBarrier(path string) error {
	return f.barrier("parent", path)
}

func (f *controlVerifyFault) barrier(kind, path string) error {
	f.barriers = append(f.barriers, kind+":"+path)
	if f.failAt == kind {
		return f.failure
	}
	return nil
}

func (f *controlVerifyFault) OnCleanupUnlink(_ string) error {
	return f.cleanupErr
}

type controlVerifyBlockingBarrier struct {
	BaseFaultInjector
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (f *controlVerifyBlockingBarrier) OnDirBarrier(_ string) error {
	f.once.Do(func() { close(f.started) })
	<-f.release
	return nil
}
