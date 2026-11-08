package taskdir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestControlCacheRemovalRequiresExactDisposableGeneration(t *testing.T) {
	_, directory := controlVerifyDir(t)
	original := controlTestRecord{Request: "original"}
	for _, name := range []string{"intent.json", "accepted.json", "rejected.json", "control-" + strings.Repeat("a", 32) + ".json", "observation-00000000000000000001.json"} {
		if _, err := directory.Put(name, original); err != nil {
			t.Fatal(err)
		}
	}
	err := directory.WithLock(t.Context(), func(tx *ControlTransaction) error {
		return tx.RemoveCacheRecord("observation", 1, controlTestRecord{Request: "different"})
	})
	if !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("mismatching cache removed: %v", err)
	}
	for _, family := range []string{"intent", "accepted", "rejected", "control", "../intent", "observation-", "123"} {
		err = directory.WithLock(t.Context(), func(tx *ControlTransaction) error { return tx.RemoveCacheRecord(family, 1, original) })
		if err == nil {
			t.Fatalf("authority record accessible through family %q", family)
		}
	}
	if err = directory.WithLock(t.Context(), func(tx *ControlTransaction) error { return tx.RemoveCacheRecord("observation", 1, original) }); err != nil {
		t.Fatal(err)
	}
	var record controlTestRecord
	if err = directory.Read("observation-00000000000000000001.json", &record); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache retained: %v", err)
	}
	for _, name := range []string{"intent.json", "accepted.json", "rejected.json", "control-" + strings.Repeat("a", 32) + ".json"} {
		if err = directory.Read(name, &record); err != nil || record != original {
			t.Fatalf("authority record %s changed: %+v %v", name, record, err)
		}
	}
}

type cacheBarrierFault struct {
	BaseFaultInjector
	fail  error
	calls int
}

func (f *cacheBarrierFault) OnCleanupDirBarrier(string) error { f.calls++; return f.fail }

func TestControlCacheRemovalReportsFailedDirectoryBarrier(t *testing.T) {
	store, directory := controlVerifyDir(t)
	record := controlTestRecord{Request: "cache"}
	name := "observation-00000000000000000001.json"
	if _, err := directory.Put(name, record); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("cache directory barrier failed")
	fault := &cacheBarrierFault{fail: failure}
	store.SetFaultInjector(fault)
	err := directory.WithLock(t.Context(), func(tx *ControlTransaction) error { return tx.RemoveCacheRecord("observation", 1, record) })
	store.SetFaultInjector(nil)
	if !errors.Is(err, failure) || fault.calls != 1 {
		t.Fatalf("barrier failure hidden: %v calls%d", err, fault.calls)
	}
	if _, err = os.Stat(filepath.Join(directory.dir, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unlinked cache not observed: %v", err)
	}
}
