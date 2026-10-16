package taskdir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func openTestDirectoryLock(t *testing.T, path string) *LockFile {
	t.Helper()
	lock, err := OpenDirectoryLock(path, LockLevelRun)
	must(t, err)
	t.Cleanup(func() { must(t, lock.Close()) })
	return lock
}

func TestDirectoryLockPreservesSharedResourcePermissions(t *testing.T) {
	path := t.TempDir()
	must(t, os.Chmod(path, 0o755))
	lock := openTestDirectoryLock(t, path)
	must(t, lock.LockEX(t.Context()))
	info, err := os.Stat(path)
	must(t, err)
	entries, err := os.ReadDir(path)
	must(t, err)
	if info.Mode().Perm() != 0o755 || len(entries) != 0 {
		t.Fatal("directory locking changed shared permissions or created metadata")
	}
}

func TestExclusiveFileTransferConsumesOriginalOwner(t *testing.T) {
	path := t.TempDir()
	lock := openTestDirectoryLock(t, path)
	must(t, lock.LockEX(t.Context()))
	file, err := lock.TakeExclusiveFile()
	must(t, err)
	t.Cleanup(func() {
		if file != nil {
			must(t, file.Close())
		}
	})
	must(t, lock.Unlock())
	must(t, lock.Close())
	if lock.Fd() != -1 {
		t.Fatal("transferred owner retained a reusable descriptor number")
	}
	if _, err = lock.TakeExclusiveFile(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("second transfer = %v, want closed owner", err)
	}
	contender := openTestDirectoryLock(t, path)
	if err = contender.LockEXNonblocking(); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("original owner unlocked its transferred lease: %v", err)
	}
	must(t, file.Close())
	file = nil
	must(t, contender.LockEX(t.Context()))
}

func TestExclusiveFileTransferRejectsUnheldAndSharedLocks(t *testing.T) {
	path := t.TempDir()
	lock := openTestDirectoryLock(t, path)
	if _, err := lock.TakeExclusiveFile(); !errors.Is(err, ErrLockNotHeld) {
		t.Fatalf("unheld transfer = %v", err)
	}
	must(t, lock.LockSH(t.Context()))
	if _, err := lock.TakeExclusiveFile(); !errors.Is(err, ErrLockNotHeld) {
		t.Fatalf("shared transfer = %v", err)
	}
	contender := openTestDirectoryLock(t, path)
	if err := contender.LockEXNonblocking(); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("failed transfer consumed its shared lease: %v", err)
	}
	must(t, lock.Unlock())
	must(t, contender.LockEX(t.Context()))
}

func TestExclusiveFileTransferRejectsChangedDirectoryIdentity(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "common")
	must(t, os.Mkdir(path, 0o755))
	lock := openTestDirectoryLock(t, path)
	must(t, lock.LockEX(t.Context()))
	must(t, os.Rename(path, filepath.Join(parent, "retained")))
	must(t, os.Mkdir(path, 0o755))
	if _, err := lock.TakeExclusiveFile(); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("transfer accepted a replaced directory: %v", err)
	}
}

func TestDirectoryLockRejectsFilesAndSymlinks(t *testing.T) {
	parent := t.TempDir()
	file := filepath.Join(parent, "file")
	link := filepath.Join(parent, "link")
	must(t, os.WriteFile(file, nil, 0o600))
	must(t, os.Symlink(parent, link))
	for _, path := range []string{file, link} {
		lock, err := OpenDirectoryLock(path, LockLevelRun)
		if err == nil {
			must(t, lock.Close())
			t.Fatalf("directory lock accepted %s", path)
		}
	}
}
