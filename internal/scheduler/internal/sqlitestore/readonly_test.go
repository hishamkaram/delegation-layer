package sqlitestore

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
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
	reader, openErr := OpenReadOnly(testContext(t), root, testRootID)
	if reader != nil {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(openErr, fs.ErrPermission) {
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
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(t.Context(), root, testRootID); !errors.Is(err, errJournalMode) {
		t.Fatalf("WAL err=%v", err)
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
