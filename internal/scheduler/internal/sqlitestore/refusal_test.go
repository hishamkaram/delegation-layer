package sqlitestore

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// foreignFixture creates a database that this store must refuse, then
// records the logical facts an independent connection must still observe.
type foreignFixture struct {
	root, path string
	mode       string
	version    int64
	rows       int64
}

func newForeignFixture(t *testing.T, statements ...string) foreignFixture {
	t.Helper()
	root := newRoot(t)
	path := DatabasePath(root)
	if err := createEmptyDatabase(path); err != nil {
		t.Fatal(err)
	}
	conn := rawConn(t, path)
	rawExec(t, conn, statements...)
	fixture := foreignFixture{root: root, path: path, mode: rawText(t, conn, "PRAGMA journal_mode"), version: rawInt(t, conn, "PRAGMA user_version")}
	fixture.rows = foreignRows(t, conn)
	closeRaw(t, conn)
	return fixture
}

func foreignRows(t *testing.T, conn *sql.Conn) int64 {
	t.Helper()
	if rawInt(t, conn, "SELECT count(*) FROM sqlite_schema WHERE name = 'foreign_data'") == 0 {
		return -1
	}
	return rawInt(t, conn, "SELECT count(*) FROM foreign_data")
}

func closeRaw(t *testing.T, conn *sql.Conn) {
	t.Helper()
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
}

// requireUntouched checks from an independent connection that a refusal
// converted nothing and wrote no scheduler objects. Physical bytes may differ:
// stock SQLite may recover or checkpoint on open and close.
func (f foreignFixture) requireUntouched(t *testing.T) {
	t.Helper()
	conn := rawConn(t, f.path)
	if mode := rawText(t, conn, "PRAGMA journal_mode"); mode != f.mode {
		t.Errorf("journal mode %q, want %q", mode, f.mode)
	}
	if version := rawInt(t, conn, "PRAGMA user_version"); version != f.version {
		t.Errorf("user_version %d, want %d", version, f.version)
	}
	if rows := foreignRows(t, conn); rows != f.rows {
		t.Errorf("foreign rows %d, want %d", rows, f.rows)
	}
	if f.version == 0 && rawInt(t, conn, "SELECT count(*) FROM sqlite_schema WHERE name = 'schedules'") != 0 {
		t.Error("scheduler DDL was added to a foreign database")
	}
}

func TestForeignWALDatabaseIsRefusedWithoutConversion(t *testing.T) {
	fixture := newForeignFixture(t, "PRAGMA journal_mode = WAL", "CREATE TABLE foreign_data (x INTEGER)", "INSERT INTO foreign_data VALUES (1), (2)")
	requireRefused(t, fixture.root, errJournalMode)
	fixture.requireUntouched(t)
}

func TestNewerWALDatabaseIsRefusedWithoutConversion(t *testing.T) {
	_, root := initializedStore(t)
	conn := rawConn(t, DatabasePath(root))
	rawExec(t, conn, "PRAGMA user_version = 3", "CREATE TABLE foreign_data (x INTEGER)", "INSERT INTO foreign_data VALUES (7)", "PRAGMA journal_mode = WAL")
	closeRaw(t, conn)
	fixture := foreignFixture{root: root, path: DatabasePath(root), mode: "wal", version: 3, rows: 1}
	requireRefused(t, root, errJournalMode)
	fixture.requireUntouched(t)
}

func TestCurrentDatabaseConvertedToWALIsRefusedNotRepaired(t *testing.T) {
	store, root := initializedStore(t)
	createSchedule(t, store, testScheduleID)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	conn := rawConn(t, DatabasePath(root))
	rawExec(t, conn, "PRAGMA journal_mode = WAL")
	closeRaw(t, conn)
	requireRefused(t, root, errJournalMode)
	check := rawConn(t, DatabasePath(root))
	if mode := rawText(t, check, "PRAGMA journal_mode"); mode != "wal" {
		t.Fatalf("journal mode %q was converted", mode)
	}
	if count := rawInt(t, check, "SELECT count(*) FROM schedules"); count != 1 {
		t.Fatalf("%d schedules after refusal", count)
	}
}

func TestNewerDatabaseIsRefusedAsUnsupported(t *testing.T) {
	_, root := initializedStore(t)
	conn := rawConn(t, DatabasePath(root))
	rawExec(t, conn, "PRAGMA user_version = 3", "CREATE TABLE foreign_data (x INTEGER)")
	closeRaw(t, conn)
	before, err := os.ReadFile(DatabasePath(root))
	if err != nil {
		t.Fatal(err)
	}
	requireRefused(t, root, errNewerSchema)
	if after, err := os.ReadFile(DatabasePath(root)); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("refusing a newer database changed its bytes: %v", err)
	}
	(foreignFixture{root: root, path: DatabasePath(root), mode: "delete", version: 3, rows: 0}).requireUntouched(t)
}

func TestHeaderBearingVersionZeroDatabaseIsForeign(t *testing.T) {
	cases := map[string][]string{
		"empty schema with header": {"CREATE TABLE foreign_data (x INTEGER)", "DROP TABLE foreign_data"},
		"foreign table":            {"CREATE TABLE foreign_data (x INTEGER)", "INSERT INTO foreign_data VALUES (3)"},
	}
	for name, statements := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newForeignFixture(t, statements...)
			if info, err := os.Stat(fixture.path); err != nil || info.Size() == 0 {
				t.Fatalf("fixture has no header: %v", err)
			}
			requireRefused(t, fixture.root, errForeignDatabase)
			fixture.requireUntouched(t)
		})
	}
}

func TestNonDatabaseFileIsForeign(t *testing.T) {
	root := newRoot(t)
	if err := os.WriteFile(DatabasePath(root), []byte("not a sqlite database, just private bytes......."), 0o600); err != nil {
		t.Fatal(err)
	}
	requireRefused(t, root, task.ErrUnsupportedSchema)
	data, err := os.ReadFile(DatabasePath(root))
	if err != nil || string(data) != "not a sqlite database, just private bytes......." {
		t.Fatalf("foreign file changed: %q %v", data, err)
	}
}

func TestSchemaTamperingIsAnInvariantFault(t *testing.T) {
	store, root := initializedStore(t)
	conn := rawConn(t, DatabasePath(root))
	rawExec(t, conn, "DROP TRIGGER schedules_permanent")
	closeRaw(t, conn)
	err := store.CreateSchedule(testContext(t), testDefinition(testScheduleID))
	if !errors.Is(err, errSchemaMismatch) || !errors.Is(err, task.ErrInvariantFault) {
		t.Fatalf("tampered schema err=%v, want schema mismatch invariant fault", err)
	}
	check := rawConn(t, DatabasePath(root))
	if count := rawInt(t, check, "SELECT count(*) FROM schedules"); count != 0 {
		t.Fatalf("write proceeded on a tampered schema: %d rows", count)
	}
}

func TestConversionToWALBetweenOperationsIsRefused(t *testing.T) {
	store, root := initializedStore(t)
	createSchedule(t, store, testScheduleID)
	conn := rawConn(t, DatabasePath(root))
	rawExec(t, conn, "PRAGMA journal_mode = WAL")
	closeRaw(t, conn)
	if err := store.CreateSchedule(testContext(t), testDefinition(otherSchedule)); !errors.Is(err, errJournalMode) {
		t.Fatalf("write after WAL conversion err=%v, want journal mode refusal", err)
	}
	if _, err := store.GetSchedule(testContext(t), testScheduleID); !errors.Is(err, errJournalMode) {
		t.Fatalf("read after WAL conversion err=%v, want journal mode refusal", err)
	}
	check := rawConn(t, DatabasePath(root))
	if mode := rawText(t, check, "PRAGMA journal_mode"); mode != "wal" {
		t.Fatalf("journal mode %q was converted back", mode)
	}
	if count := rawInt(t, check, "SELECT count(*) FROM schedules"); count != 1 {
		t.Fatalf("%d schedules after refused write", count)
	}
}
