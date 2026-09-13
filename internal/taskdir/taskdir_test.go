package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

type testFaultInjector struct {
	BaseFaultInjector
	failStageWrite   bool
	failStageBarrier bool
	failStageClose   bool
	failPostLinkDir  bool
	failCleanup      bool
}

func (t *testFaultInjector) OnStageWrite(_ string, data []byte) (int, error) {
	if t.failStageWrite {
		return 0, errors.New("injected stage write error")
	}
	return len(data), nil
}

func (t *testFaultInjector) OnStageBarrier(_ string) error {
	if t.failStageBarrier {
		return errors.New("injected stage barrier error")
	}
	return nil
}

func (t *testFaultInjector) OnStageClose(_ string) error {
	if t.failStageClose {
		return errors.New("injected stage close error")
	}
	return nil
}

func (t *testFaultInjector) OnPostLinkDirBarrier(_ string) error {
	if t.failPostLinkDir {
		return errors.New("injected post link dir barrier error")
	}
	return nil
}

func (t *testFaultInjector) OnCleanupUnlink(_ string) error {
	if t.failCleanup {
		return errors.New("injected cleanup unlink error")
	}
	return nil
}

func closeStoreQuietly(s *Store) {
	if err := s.Close(); err != nil {
		return
	}
}

func TestStoreInitAndOpen(t *testing.T) {
	tmpDir := t.TempDir()
	storeRoot := filepath.Join(tmpDir, "state-store")

	// 1. Init
	s, initErr := InitStore(storeRoot)
	if initErr != nil {
		t.Fatalf("InitStore failed: %v", initErr)
	}
	defer closeStoreQuietly(s)

	if s.RootID == "" {
		t.Fatalf("expected non-empty RootID")
	}

	// 2. Probe filesystem
	if probeErr := s.ProbeFilesystemSupport(); probeErr != nil {
		t.Fatalf("ProbeFilesystemSupport failed: %v", probeErr)
	}

	// 3. Re-open
	s2, openErr := OpenStore(storeRoot)
	if openErr != nil {
		t.Fatalf("OpenStore failed: %v", openErr)
	}
	defer closeStoreQuietly(s2)

	if s2.RootID != s.RootID {
		t.Errorf("expected RootID %s, got %s", s.RootID, s2.RootID)
	}
}

func TestLockFileOrderingAndCloexec(t *testing.T) {
	tmpDir := t.TempDir()
	lockPath := filepath.Join(tmpDir, ".test.lock")

	l1, openErr := OpenLockFile(lockPath, LockLevelAdmission)
	if openErr != nil {
		t.Fatalf("OpenLockFile failed: %v", openErr)
	}
	defer closeLockQuietly(l1)

	// Verify FD_CLOEXEC
	flags, fcntlErr := unix.FcntlInt(uintptr(l1.Fd()), unix.F_GETFD, 0)
	if fcntlErr != nil {
		t.Fatal(fcntlErr)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Errorf("expected FD_CLOEXEC set on lock fd")
	}

	// Shared lock
	if shErr := l1.LockSH(context.Background()); shErr != nil {
		t.Fatalf("LockSH failed: %v", shErr)
	}

	// Open second handle on same lock file
	l2, l2Err := OpenLockFile(lockPath, LockLevelAdmission)
	if l2Err != nil {
		t.Fatalf("OpenLockFile l2 failed: %v", l2Err)
	}
	defer closeLockQuietly(l2)

	// Inode must be identical
	if l1.Inode() != l2.Inode() {
		t.Errorf("expected stable inode across lock opens: %d vs %d", l1.Inode(), l2.Inode())
	}

	// Another SH lock succeeds concurrently
	if concErr := l2.LockSH(context.Background()); concErr != nil {
		t.Fatalf("concurrent LockSH failed: %v", concErr)
	}

	// EX lock from l2 must fail with ErrLockBusy while l1 holds SH
	if unErr := l2.Unlock(); unErr != nil {
		t.Fatal(unErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if exErr := l2.LockEX(ctx); !errors.Is(exErr, task.ErrLockBusy) {
		t.Errorf("expected ErrLockBusy for LockEX while SH held, got: %v", exErr)
	}

	// Release l1, now l2 EX succeeds
	if un1Err := l1.Unlock(); un1Err != nil {
		t.Fatal(un1Err)
	}
	if exSuccErr := l2.LockEX(context.Background()); exSuccErr != nil {
		t.Fatalf("LockEX after unlock failed: %v", exSuccErr)
	}
}

func setupStagingTest(t *testing.T) (*Store, string) {
	tmpDir := t.TempDir()
	s, sErr := InitStore(filepath.Join(tmpDir, "store"))
	if sErr != nil {
		t.Fatal(sErr)
	}
	testDir := filepath.Join(s.Root, "test-staging")
	if dirErr := os.MkdirAll(testDir, 0o700); dirErr != nil {
		t.Fatal(dirErr)
	}
	return s, testDir
}

func TestStagingWriteFailure(t *testing.T) {
	s, testDir := setupStagingTest(t)
	defer closeStoreQuietly(s)

	inj := &testFaultInjector{failStageWrite: true}
	committed, cleanErr, wErr := s.stageAndCommit(testDir, "test1.json", []byte("data"), inj)
	if cleanErr != nil {
		t.Fatalf("unexpected cleanup error: %v", cleanErr)
	}
	if wErr == nil || committed {
		t.Errorf("expected error on injected write failure, got committed=%v", committed)
	}
	if _, statErr := os.Stat(filepath.Join(testDir, "test1.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("dest should not exist on write failure")
	}
}

func TestStagingBarrierFailure(t *testing.T) {
	s, testDir := setupStagingTest(t)
	defer closeStoreQuietly(s)

	inj := &testFaultInjector{failStageBarrier: true}
	committed, cleanErr, bErr := s.stageAndCommit(testDir, "test2.json", []byte("data"), inj)
	if cleanErr != nil {
		t.Fatalf("unexpected cleanup error: %v", cleanErr)
	}
	if bErr == nil || committed {
		t.Errorf("expected error on injected stage barrier, got committed=%v", committed)
	}
}

func TestStagingPostLinkBarrierFailure(t *testing.T) {
	s, testDir := setupStagingTest(t)
	defer closeStoreQuietly(s)

	inj := &testFaultInjector{failPostLinkDir: true}
	_, cleanErr, dErr := s.stageAndCommit(testDir, "test3.json", []byte("data"), inj)
	if cleanErr != nil {
		t.Fatalf("unexpected cleanup error: %v", cleanErr)
	}
	if !errors.Is(dErr, task.ErrUncertainDurability) {
		t.Errorf("expected ErrUncertainDurability, got: %v", dErr)
	}
	if _, statErr := os.Stat(filepath.Join(testDir, "test3.json")); statErr != nil {
		t.Errorf("destination must be preserved on uncertain durability: %v", statErr)
	}
}

func TestStagingCleanupFailure(t *testing.T) {
	s, testDir := setupStagingTest(t)
	defer closeStoreQuietly(s)

	inj := &testFaultInjector{failCleanup: true}
	committed, cleanErr, sErr := s.stageAndCommit(testDir, "test4.json", []byte("data"), inj)
	if sErr != nil || !committed {
		t.Errorf("expected committed true on cleanup failure, got err=%v", sErr)
	}
	if cleanErr == nil {
		t.Errorf("expected cleanErr to be reported")
	}
	if _, statErr := os.Stat(filepath.Join(testDir, "test4.json")); statErr != nil {
		t.Errorf("destination must be committed: %v", statErr)
	}
}

func TestScavengerRespectsActiveRecords(t *testing.T) {
	tmpDir := t.TempDir()
	s, sErr := InitStore(filepath.Join(tmpDir, "store"))
	if sErr != nil {
		t.Fatal(sErr)
	}
	defer closeStoreQuietly(s)

	taskDir := filepath.Join(s.Root, "tasks", "0123456789abcdef0123456789abcdef")
	if dirErr := os.MkdirAll(taskDir, 0o700); dirErr != nil {
		t.Fatal(dirErr)
	}

	// Create valid records and locks
	realFiles := []string{
		"task.json",
		"meta.json",
		"brief.md",
		"submit.json",
		"provider.start",
		"result.txt",
		"outcome.json",
		".admission.lock",
		".run.lock",
	}
	for _, name := range realFiles {
		p := filepath.Join(taskDir, name)
		if wErr := os.WriteFile(p, []byte("real content"), 0o600); wErr != nil {
			t.Fatal(wErr)
		}
	}

	// Create abandoned stage files
	abandonedStage := filepath.Join(taskDir, "stage.0123456789abcdef0123456789abcdef.tmp")
	if wErr := os.WriteFile(abandonedStage, []byte("stage content"), 0o600); wErr != nil {
		t.Fatal(wErr)
	}
	unrelatedFile := filepath.Join(taskDir, "other.tmp")
	if wErr := os.WriteFile(unrelatedFile, []byte("other"), 0o600); wErr != nil {
		t.Fatal(wErr)
	}

	// Run scavenger
	cleaned, scavErr := s.Scavenge()
	if scavErr != nil {
		t.Fatalf("Scavenge failed: %v", scavErr)
	}
	if cleaned != 1 {
		t.Errorf("expected 1 cleaned file, got %d", cleaned)
	}

	// Abandoned stage file is removed
	if _, statErr := os.Stat(abandonedStage); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("expected abandoned stage to be removed")
	}

	// Unrelated and real files are preserved
	if _, statErr := os.Stat(unrelatedFile); statErr != nil {
		t.Errorf("unrelated file should be preserved")
	}
	for _, name := range realFiles {
		if _, statErr := os.Stat(filepath.Join(taskDir, name)); statErr != nil {
			t.Errorf("authoritative file %s must be preserved", name)
		}
	}
}
