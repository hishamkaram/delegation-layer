package sqlitestore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

func TestReadOnlyStoreLeavesHotJournalRecoveryToWriter(t *testing.T) {
	root := newRoot(t)
	if err := Initialize(testContext(t), root, testRootID); err != nil {
		t.Fatal(err)
	}
	path := DatabasePath(root)
	interrupt(t, path, "populated")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(path + "-journal")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := readOnlyFiles(t, root)
	reader, openErr := OpenReadOnly(testContext(t), root, testRootID)
	if reader != nil {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if CompatibilityOf(openErr) != CompatibilityObservationIncomplete || !errors.Is(openErr, fs.ErrPermission) {
		t.Fatalf("hot-journal read=%v; want explicit read-only recovery refusal", openErr)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("passive open changed database: %v", err)
	}
	afterJournal, err := os.ReadFile(path + "-journal")
	if err != nil || !bytes.Equal(journal, afterJournal) {
		t.Fatalf("passive open recovered/deleted hot journal: %v", err)
	}
	assertReadOnlyFiles(t, root, snapshot)
}

func TestReadOnlyStoreCannotMutateAndDoesNotTakeWriterReservation(t *testing.T) {
	writer, root := initializedStore(t)
	createSchedule(t, writer, testScheduleID)
	conn := rawConn(t, DatabasePath(root))
	rawExec(t, conn, "BEGIN IMMEDIATE")
	t.Cleanup(func() { rawExec(t, conn, "ROLLBACK") })
	// A RESERVED writer lock allows reads. The former immediate connector
	// would instead wait for that writer and miss this finite read deadline.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	reader, err := OpenReadOnly(ctx, root, testRootID)
	if err != nil {
		t.Fatalf("read beside writer: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	page, err := reader.ListSchedules(ctx, contract.SchedulePage{Limit: 10})
	if err != nil || len(page.Schedules) != 1 {
		t.Fatalf("list beside writer: %+v, %v", page, err)
	}
	if err := reader.CreateSchedule(ctx, testDefinition(otherSchedule)); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("read-only mutation err=%v; want permission refusal", err)
	}
}

func TestReadOnlyStoreRefusesWALBeforeSQLiteCreatesSidecars(t *testing.T) {
	root := newRoot(t)
	if err := Initialize(testContext(t), root, testRootID); err != nil {
		t.Fatal(err)
	}
	// Header format versions are defined by SQLite: 2 means WAL. No live
	// connection exists while this deliberately unsupported fixture is made.
	file, err := os.OpenFile(DatabasePath(root), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteAt([]byte{2, 2}, 18)
	if closeErr := errors.Join(writeErr, file.Close()); closeErr != nil {
		t.Fatal(closeErr)
	}
	before, err := os.ReadFile(DatabasePath(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, openErr := OpenReadOnly(t.Context(), root, testRootID); !errors.Is(openErr, errJournalMode) || CompatibilityOf(openErr) != CompatibilityIncompatible {
		t.Fatalf("WAL err=%v", openErr)
	}
	after, err := os.ReadFile(DatabasePath(root))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("WAL refusal changed database: %v", err)
	}
	for _, suffix := range []string{"-shm", "-wal", "-journal"} {
		if _, err := os.Lstat(DatabasePath(root) + suffix); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("read-only open created %s: %v", suffix, err)
		}
	}
}

func TestReadOnlyStorePreservesMissingAndEmptyClassification(t *testing.T) {
	root := newRoot(t)
	if _, err := OpenReadOnly(t.Context(), root, testRootID); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing database: %v", err)
	}
	if err := createEmptyDatabase(DatabasePath(root)); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(t.Context(), root, testRootID); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("empty database: %v", err)
	}
}

func TestCompatibilityOfPreservesAllErrorCauses(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want Compatibility
	}{
		{"current", nil, CompatibilityCompatible},
		{"wrapper without cause", fmt.Errorf("missing cause: %w", nil), CompatibilityObservationIncomplete},
		{"absent", fmt.Errorf("read: %w", errNotInitialized), CompatibilityAbsent},
		{"migration", errors.Join(fmt.Errorf("read: %w", contract.ErrMigrationRequired)), CompatibilityMigrationRequired},
		{"newer", errors.Join(fmt.Errorf("read: %w", errNewerSchema)), CompatibilityNewerSchema},
		{"foreign", fmt.Errorf("read: %w", errForeignDatabase), CompatibilityIncompatible},
		{"journal", errors.Join(fmt.Errorf("read: %w", errJournalMode)), CompatibilityIncompatible},
		{"newer and close fault", errors.Join(errNewerSchema, errors.New("close failed")), CompatibilityObservationIncomplete},
		{"wrapped newer and close fault", fmt.Errorf("observation: %w", errors.Join(errNewerSchema, errors.New("close failed"))), CompatibilityObservationIncomplete},
		{"absent and permission", errors.Join(errNotInitialized, fs.ErrPermission), CompatibilityObservationIncomplete},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := CompatibilityOf(test.err); got != test.want {
				t.Fatalf("got %s, want %s", got, test.want)
			}
		})
	}
}

func TestReadOnlyCompatibilityPreservesDatabase(t *testing.T) {
	for _, test := range []struct {
		name    string
		version int
		legacy  bool
		want    Compatibility
	}{
		{name: "current", want: CompatibilityCompatible},
		{name: "legacy", legacy: true, want: CompatibilityMigrationRequired},
		{name: "newer", version: 3, want: CompatibilityNewerSchema},
		{name: "foreign", version: -1, want: CompatibilityIncompatible},
	} {
		t.Run(test.name, func(t *testing.T) {
			var root string
			if test.legacy {
				root = legacyDatabase(t)
			} else {
				root = newRoot(t)
				if err := Initialize(t.Context(), root, testRootID); err != nil {
					t.Fatal(err)
				}
				if test.version != 0 {
					rawExec(t, rawConn(t, DatabasePath(root)), fmt.Sprintf("PRAGMA user_version = %d", test.version))
				}
			}
			snapshot := readOnlyFiles(t, root)
			reader, openErr := OpenReadOnly(t.Context(), root, testRootID)
			if reader != nil {
				openErr = errors.Join(openErr, reader.Close())
			}
			if got := CompatibilityOf(openErr); got != test.want {
				t.Fatalf("compatibility=%s want=%s err=%v", got, test.want, openErr)
			}
			assertReadOnlyFiles(t, root, snapshot)
		})
	}
}

func TestReadOnlyBusyCompatibilityIsIncomplete(t *testing.T) {
	root := newRoot(t)
	if err := Initialize(t.Context(), root, testRootID); err != nil {
		t.Fatal(err)
	}
	conn := rawConn(t, DatabasePath(root))
	rawExec(t, conn, "BEGIN EXCLUSIVE")
	t.Cleanup(func() { rawExec(t, conn, "ROLLBACK") })
	snapshot := readOnlyFiles(t, root)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	reader, openErr := OpenReadOnly(ctx, root, testRootID)
	if reader != nil {
		openErr = errors.Join(openErr, reader.Close())
	}
	if CompatibilityOf(openErr) != CompatibilityObservationIncomplete {
		t.Fatalf("busy observation=%v", openErr)
	}
	assertReadOnlyFiles(t, root, snapshot)
}

// A snapshot covers both persisted bytes and the complete set of sidecars.
func readOnlyFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(Directory(root))
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		contents, err := os.ReadFile(filepath.Join(Directory(root), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = contents
	}
	return files
}

func assertReadOnlyFiles(t *testing.T, root string, before map[string][]byte) {
	t.Helper()
	if !maps.EqualFunc(before, readOnlyFiles(t, root), bytes.Equal) {
		t.Fatal("passive observation changed scheduler bytes or directory entries")
	}
}
