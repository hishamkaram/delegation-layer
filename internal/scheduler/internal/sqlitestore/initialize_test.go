package sqlitestore

import (
	"errors"
	"io/fs"
	"os"
	"reflect"
	"runtime"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInitializeCreatesPrivateCurrentDeleteJournalDatabase(t *testing.T) {
	root := newRoot(t)
	if err := Initialize(testContext(t), root, testRootID); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	requireMode(t, Directory(root), fs.ModeDir|0o700)
	requireMode(t, DatabasePath(root), 0o600)
	conn := rawConn(t, DatabasePath(root))
	if mode := rawText(t, conn, "PRAGMA journal_mode"); mode != "delete" {
		t.Fatalf("journal mode %q, want delete without any setter", mode)
	}
	if version := rawInt(t, conn, "PRAGMA user_version"); version != schemaVersion {
		t.Fatalf("user_version %d", version)
	}
	if strict := rawInt(t, conn, "SELECT count(*) FROM pragma_table_list WHERE schema = 'main' AND strict = 1"); strict != 10 {
		t.Fatalf("%d STRICT tables, want exactly 10", strict)
	}
	if tables := rawInt(t, conn, "SELECT count(*) FROM sqlite_schema WHERE type = 'table'"); tables != 10 {
		t.Fatalf("%d tables, want exactly 10", tables)
	}
	if root := rawText(t, conn, "SELECT root_id FROM scheduler_metadata"); root != testRootID {
		t.Fatalf("root %q", root)
	}
}

func TestInitializeIsRepeatableAndPreservesData(t *testing.T) {
	store, root := mixedHistory(t)
	if err := store.RecordObservation(testContext(t), testObservation(testTaskID, 2_050)); err != nil {
		t.Fatal(err)
	}
	before := readState(t, store)
	for range 2 {
		if err := Initialize(testContext(t), root, testRootID); err != nil {
			t.Fatalf("repeated initialize: %v", err)
		}
	}
	if after := readState(t, store); !reflect.DeepEqual(after, before) {
		t.Fatalf("repeated initialization changed the data:\nbefore %+v\nafter  %+v", before, after)
	}
	if err := Initialize(testContext(t), root, otherRootID); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("initialize for another root err=%v, want ErrIdentityMismatch", err)
	}
}

// storedState is every read the store offers for the mixed-history fixture.
type storedState struct {
	schedule     contract.Schedule
	released     contract.Occurrence
	active       contract.Occurrence
	history      contract.HistoryPageResult
	activePage   contract.ActivePageResult
	schedulePage contract.SchedulePageResult
}

func readState(t *testing.T, store *Store) storedState {
	t.Helper()
	ctx := testContext(t)
	var state storedState
	var errs [6]error
	state.schedule, errs[0] = store.GetSchedule(ctx, testScheduleID)
	state.released, errs[1] = store.GetOccurrence(ctx, testTaskID)
	state.active, errs[2] = store.GetOccurrence(ctx, otherTaskID)
	state.history, errs[3] = store.History(ctx, contract.HistoryPage{ScheduleID: testScheduleID, Limit: contract.MaxPageLimit})
	state.activePage, errs[4] = store.ActiveOccurrences(ctx, contract.ActivePage{Limit: contract.MaxPageLimit})
	state.schedulePage, errs[5] = store.ListSchedules(ctx, contract.SchedulePage{Limit: contract.MaxPageLimit})
	if err := errors.Join(errs[:]...); err != nil {
		t.Fatal(err)
	}
	if state.released.Release == nil || state.released.Observation == nil || len(state.history.Entries) != 4 {
		t.Fatalf("fixture lacks a released, observed occurrence and skip spans: %+v", state)
	}
	return state
}

func TestFailedInitializationRollsBackAndStaysEligible(t *testing.T) {
	root := newRoot(t)
	path := DatabasePath(root)
	if err := createEmptyDatabase(path); err != nil {
		t.Fatal(err)
	}
	failing := append(schemaObjects(), schemaObject{kind: "table", name: "schedules", sql: "CREATE TABLE schedules (x INTEGER) STRICT"})
	err := initializeWithConnector(testContext(t), storeConnector(t, path), path, testRootID, runtime.GOOS, failing)
	if err == nil {
		t.Fatal("duplicate DDL accepted")
	}
	conn := rawConn(t, path)
	if objects := rawInt(t, conn, "SELECT count(*) FROM sqlite_schema"); objects != 0 {
		t.Fatalf("failed initialization left %d schema objects", objects)
	}
	if info, statErr := os.Stat(path); statErr != nil || info.Size() != 0 {
		t.Fatalf("failed initialization left a nonempty file: %v", statErr)
	}
	if err := Initialize(testContext(t), root, testRootID); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	openStore(t, root)
}

func TestOpenNeverCreatesOrInitializes(t *testing.T) {
	root := newRoot(t)
	if _, err := Open(testContext(t), root, testRootID); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing database err=%v, want fs.ErrNotExist", err)
	}
	if _, err := os.Lstat(DatabasePath(root)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open created the database: %v", err)
	}
	if err := createEmptyDatabase(DatabasePath(root)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(testContext(t), root, testRootID); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("empty database err=%v, want fs.ErrNotExist", err)
	}
	if info, err := os.Stat(DatabasePath(root)); err != nil || info.Size() != 0 {
		t.Fatalf("Open wrote the empty database: %v", err)
	}
	if _, err := Open(testContext(t), t.TempDir(), testRootID); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing directory err=%v, want fs.ErrNotExist", err)
	}
}

func TestOpenRefusesUnsafeFilesAndWrongRoot(t *testing.T) {
	_, root := initializedStore(t)
	if _, err := Open(testContext(t), root, otherRootID); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("other root err=%v, want ErrIdentityMismatch", err)
	}
	path := DatabasePath(root)
	for _, mode := range []fs.FileMode{0o644, 0o400} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(testContext(t), root, testRootID); !errors.Is(err, task.ErrEvidenceFault) {
			t.Errorf("mode %v err=%v, want ErrEvidenceFault", mode, err)
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(Directory(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(testContext(t), root, testRootID); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("shared directory err=%v, want ErrEvidenceFault", err)
	}
	if err := os.Chmod(Directory(root), 0o700); err != nil {
		t.Fatal(err)
	}
	linkedRoot := newRoot(t)
	if err := os.Symlink(path, DatabasePath(linkedRoot)); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(testContext(t), linkedRoot, testRootID); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("symlinked database err=%v, want ErrEvidenceFault", err)
	}
}

func requireMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode() & (fs.ModeType | fs.ModePerm); got != want {
		t.Fatalf("%s mode %v, want %v", path, got, want)
	}
}

func requireRefused(t *testing.T, root string, want error) {
	t.Helper()
	_, openErr := Open(testContext(t), root, testRootID)
	initErr := Initialize(testContext(t), root, testRootID)
	for name, err := range map[string]error{"open": openErr, "initialize": initErr} {
		if !errors.Is(err, want) {
			t.Errorf("%s err=%v, want %v", name, err, want)
		}
	}
}

func TestConcurrentInitializationsSerializeToOneSchema(t *testing.T) {
	root := newRoot(t)
	const initializers = 4
	errs := make(chan error, initializers)
	start := make(chan struct{})
	for range initializers {
		ctx := testContext(t)
		go func() {
			<-start
			errs <- Initialize(ctx, root, testRootID)
		}()
	}
	close(start)
	for range initializers {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent initialize: %v", err)
		}
	}
	store := openStore(t, root)
	createSchedule(t, store, testScheduleID)
}

// The close fault itself has no injection seam; this checks the decision
// that joins it with a barrier fault.
func TestNewDatabaseBarrierAndCloseFaultsAreBothReported(t *testing.T) {
	barrier, closing := errors.New("barrier failed"), errors.New("close failed")
	if err := closeAfterBarrier(barrier, closing); !errors.Is(err, barrier) || !errors.Is(err, closing) {
		t.Fatalf("both faults err=%v, want both", err)
	}
	if err := closeAfterBarrier(nil, closing); !errors.Is(err, closing) {
		t.Fatalf("close fault err=%v", err)
	}
	if err := closeAfterBarrier(barrier, nil); !errors.Is(err, barrier) {
		t.Fatalf("barrier fault err=%v", err)
	}
	if err := closeAfterBarrier(nil, nil); err != nil {
		t.Fatalf("no fault err=%v", err)
	}
}
