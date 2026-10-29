package scheduler

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/internal/sqlitestore"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// fixtureExpectation is what one CLI fixture state must show while held and
// after release.
type fixtureExpectation struct {
	whileHeld, afterRelease error
	probeHeld, probeAfter   string
}

var fixtureExpectations = map[string]fixtureExpectation{
	fixtureMissing: {whileHeld: fs.ErrNotExist, afterRelease: fs.ErrNotExist, probeHeld: "missing", probeAfter: "missing"},
	fixtureCorrupt: {whileHeld: task.ErrUnsupportedSchema, afterRelease: task.ErrUnsupportedSchema, probeHeld: "unsupported", probeAfter: "unsupported"},
	fixtureNewer:   {whileHeld: task.ErrUnsupportedSchema, afterRelease: task.ErrUnsupportedSchema, probeHeld: "unsupported", probeAfter: "unsupported"},
	fixtureLocked:  {whileHeld: task.ErrLockBusy, probeHeld: "busy", probeAfter: "open"},
}

func TestCLIFixturesHoldTheirStateAndAuthority(t *testing.T) {
	for mode, want := range fixtureExpectations {
		t.Run(mode, func(t *testing.T) {
			store := stateStore(t)
			holder := startHelper(t, mode, store.Root)
			requireLine(t, holder, "ready")
			if _, err := AcquireOwnerLease(testContext(t), store); !errors.Is(err, task.ErrLockBusy) {
				t.Fatalf("lease while the fixture holds it err=%v, want task.ErrLockBusy", err)
			}
			requireStoreClass(t, store, want.whileHeld, want.probeHeld)
			if mode == fixtureLocked {
				requireSQLBusy(t, store.Root)
			}
			held := databaseBytes(t, store.Root)
			holder.Release(t)
			if code := holder.Finish(t); code != 0 {
				t.Fatalf("fixture exited %d", code)
			}
			if after := databaseBytes(t, store.Root); !bytes.Equal(after, held) {
				t.Fatal("releasing the fixture changed the database bytes")
			}
			requireStoreClass(t, store, want.afterRelease, want.probeAfter)
			acquire(t, store)
		})
	}
}

func TestCorruptAndNewerFixturesStoreTheirExactState(t *testing.T) {
	store := stateStore(t)
	holder := startHelper(t, fixtureCorrupt, store.Root)
	requireLine(t, holder, "ready")
	if got := databaseBytes(t, store.Root); !bytes.Equal(got, corruptDatabase) {
		t.Fatalf("corrupt fixture stored %q", got)
	}
	requireMode(t, sqlitestore.DatabasePath(store.Root), 0o600)
	if code := holder.Finish(t); code != 0 {
		t.Fatalf("fixture exited %d", code)
	}
	newer := stateStore(t)
	holder = startHelper(t, fixtureNewer, newer.Root)
	requireLine(t, holder, "ready")
	if code := holder.Finish(t); code != 0 {
		t.Fatalf("fixture exited %d", code)
	}
	db, conn, err := rawConnection(testContext(t), newer.Root)
	if err != nil {
		t.Fatal(err)
	}
	var version int64
	scanErr := conn.QueryRowContext(testContext(t), "PRAGMA user_version").Scan(&version)
	if err := errors.Join(scanErr, conn.Close(), db.Close()); err != nil || version != unsupportedVersion {
		t.Fatalf("newer fixture user_version %d, %v", version, err)
	}
}

func requireStoreClass(t *testing.T, store *taskdir.Store, want error, probe string) {
	t.Helper()
	opened, err := Open(testContext(t), store)
	if want == nil {
		if err != nil {
			t.Fatalf("open err=%v, want an open store", err)
		}
		if closeErr := opened.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	} else if !errors.Is(err, want) {
		t.Fatalf("open err=%v, want %v", err, want)
	}
	child := startHelper(t, probeStore, store.Root)
	requireLine(t, child, probe)
	if code := child.Finish(t); code != 0 {
		t.Fatalf("store probe exited %d", code)
	}
}

// requireSQLBusy is the positive control that the fixture holds a real SQLite
// lock: an independent connection cannot take the write lock.
func requireSQLBusy(t *testing.T, root string) {
	t.Helper()
	err := rawStatements(testContext(t), root, "BEGIN IMMEDIATE", "ROLLBACK")
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != sqlite3.SQLITE_BUSY {
		t.Fatalf("independent write lock err=%v, want SQLITE_BUSY", err)
	}
}

func databaseBytes(t *testing.T, root string) []byte {
	t.Helper()
	data, err := os.ReadFile(sqlitestore.DatabasePath(root))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func requireMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || info.Mode() != want {
		t.Fatalf("%s mode %v, %v; want %v", path, info, err, want)
	}
}
