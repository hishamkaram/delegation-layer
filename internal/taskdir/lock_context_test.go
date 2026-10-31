package taskdir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// cancelOnceExists reports cancellation only after path exists, so it is live
// while the lock's parent is prepared and done once that preparation finishes.
type cancelOnceExists struct {
	context.Context
	path string
}

func (c cancelOnceExists) Err() error {
	if _, err := os.Lstat(c.path); err == nil {
		return context.Canceled
	}
	return nil
}

func requireAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists after a refused open: %v", path, err)
	}
}

func TestOpenLockFileContextRefusesDoneContextsBeforeCreatingAnything(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancelExpired()
	var absent context.Context
	cases := map[string]struct {
		ctx  context.Context
		want error
	}{
		"nil":      {ctx: absent, want: context.Canceled},
		"canceled": {ctx: canceled, want: context.Canceled},
		"expired":  {ctx: expired, want: context.DeadlineExceeded},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			missing := filepath.Join(base, "missing")
			lock, err := OpenLockFileContext(tc.ctx, filepath.Join(missing, "nested", "owner.lock"), LockLevelNone)
			if !errors.Is(err, tc.want) || lock != nil {
				t.Fatalf("open = %v, %v; want %v", lock, err, tc.want)
			}
			requireAbsent(t, missing)
			existing, err := OpenLockFileContext(tc.ctx, filepath.Join(base, "owner.lock"), LockLevelNone)
			if !errors.Is(err, tc.want) || existing != nil {
				t.Fatalf("open beside an existing parent = %v, %v; want %v", existing, err, tc.want)
			}
			requireAbsent(t, filepath.Join(base, "owner.lock"))
		})
	}
}

func TestOpenLockFileContextChecksCancellationBeforeTheInodeOpen(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "scheduler")
	path := filepath.Join(parent, "owner.lock")
	ctx := cancelOnceExists{Context: context.Background(), path: parent}
	lock, err := OpenLockFileContext(ctx, path, LockLevelNone)
	if !errors.Is(err, context.Canceled) || lock != nil {
		t.Fatalf("open = %v, %v; want context.Canceled after parent preparation", lock, err)
	}
	info, err := os.Lstat(parent)
	if err != nil || info.Mode() != os.ModeDir|0o700 {
		t.Fatalf("prepared parent %v, %v; want a private directory", info, err)
	}
	requireAbsent(t, path)
}

func TestOpenLockFileContextCreatesAndReopensOneInode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "owner.lock")
	first, err := OpenLockFileContext(context.Background(), path, LockLevelNone)
	if err != nil {
		t.Fatal(err)
	}
	inode := first.Inode()
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode() != 0o600 {
		t.Fatalf("lock file %v, %v; want mode 0600", info, err)
	}
	reopened, err := OpenLockFileContext(context.Background(), path, LockLevelNone)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLockQuietly(reopened)
	wrapped, err := OpenLockFile(path, LockLevelNone)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLockQuietly(wrapped)
	if reopened.Inode() != inode || wrapped.Inode() != inode {
		t.Fatalf("inodes %d and %d, want the created %d", reopened.Inode(), wrapped.Inode(), inode)
	}
}

func TestOpenLockFileContextOpensAnExistingExclusiveCreateWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.lock")
	winner, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err = errors.Join(unix.Fstat(int(winner.Fd()), &stat), winner.Close()); err != nil {
		t.Fatal(err)
	}
	lock, err := OpenLockFileContext(context.Background(), path, LockLevelNone)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLockQuietly(lock)
	if lock.Inode() != uint64(stat.Ino) {
		t.Fatalf("lock inode %d is not the existing winner %d", lock.Inode(), stat.Ino)
	}
}

func TestOpenLockFileContextKeepsParentRefusals(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(base, "shared.lock")
	if err := os.WriteFile(shared, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(file, "owner.lock"), filepath.Join(link, "owner.lock"), shared} {
		lock, err := OpenLockFileContext(context.Background(), path, LockLevelNone)
		_, wrapperErr := OpenLockFile(path, LockLevelNone)
		if err == nil || lock != nil || wrapperErr == nil || err.Error() != wrapperErr.Error() {
			t.Errorf("%s: context open %v, %v; wrapper %v; want the same refusal", path, lock, err, wrapperErr)
		}
	}
	requireAbsent(t, filepath.Join(target, "owner.lock"))
}
