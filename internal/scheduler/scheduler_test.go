package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/internal/sqlitestore"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/internal/testchild"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
)

const (
	testScheduleID = "11111111111111111111111111111111"
	testTaskID     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	t.Cleanup(cancel)
	return ctx
}

func stateStore(t *testing.T) *taskdir.Store {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close state store: %v", err)
		}
	})
	return store
}

func acquire(t *testing.T, store *taskdir.Store) *OwnerLease {
	t.Helper()
	lease, err := AcquireOwnerLease(testContext(t), store)
	if err != nil {
		t.Fatalf("acquire owner lease: %v", err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Errorf("close lease: %v", err)
		}
	})
	return lease
}

func migrated(t *testing.T, store *taskdir.Store) {
	t.Helper()
	lease := acquire(t, store)
	if err := lease.Migrate(testContext(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func openScheduler(t *testing.T, store *taskdir.Store) contract.Store {
	t.Helper()
	opened, err := Open(testContext(t), store)
	if err != nil {
		t.Fatalf("open scheduler: %v", err)
	}
	t.Cleanup(func() {
		if err := opened.Close(); err != nil {
			t.Errorf("close scheduler: %v", err)
		}
	})
	return opened
}

func testDefinition() contract.Definition {
	return contract.Definition{
		DefinitionHeader: contract.DefinitionHeader{
			ScheduleID: testScheduleID,
			CreatedAt:  time.Unix(1_000, 0).UTC(),
			Cron:       "0 9 * * 1-5",
			Timezone:   "UTC",
			Request:    contract.Request{Auto: true, CanonicalCwd: "/work/repository", Config: task.TaskConfig{Permission: "read-only"}},
		},
		Brief: []byte("summarize open issues"),
	}
}

func TestOpenNeverCreatesTheDatabase(t *testing.T) {
	store := stateStore(t)
	if _, err := Open(testContext(t), store); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("open before migration err=%v, want fs.ErrNotExist", err)
	}
	if _, err := os.Lstat(sqlitestore.Directory(store.Root)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("open created the scheduler directory: %v", err)
	}
	if _, err := Open(testContext(t), nil); err == nil {
		t.Fatal("open accepted a nil state store")
	}
}

func TestOwnerLeaseIsExclusiveAndMigrateNeedsItHeld(t *testing.T) {
	store := stateStore(t)
	lease := acquire(t, store)
	if _, err := AcquireOwnerLease(testContext(t), store); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("second lease err=%v, want task.ErrLockBusy", err)
	}
	if err := lease.Migrate(testContext(t)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := lease.Migrate(testContext(t)); err != nil {
		t.Fatalf("repeated migrate: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Migrate(testContext(t)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("migrate after close err=%v, want os.ErrClosed", err)
	}
	info, err := os.Lstat(sqlitestore.Directory(store.Root))
	if err != nil || info.Mode() != fs.ModeDir|0o700 {
		t.Fatalf("scheduler directory %v, %v; want private directory", info, err)
	}
	acquire(t, store)
}

func TestLeaseDescriptorIsNotInherited(t *testing.T) {
	store := stateStore(t)
	lease := acquire(t, store)
	held, ok := lease.lock.(*taskdir.LockFile)
	if !ok {
		t.Fatalf("lease holds %T, want *taskdir.LockFile", lease.lock)
	}
	flags, err := unix.FcntlInt(uintptr(held.Fd()), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("lease descriptor flags %#x, %v; want FD_CLOEXEC", flags, err)
	}
	// Positive control: the probe sees the lease busy while this process
	// holds it.
	control := startHelper(t, helperProbe, store.Root)
	requireLine(t, control, "ready")
	control.Release(t)
	requireLine(t, control, "busy")
	// A probe started while the lease is held acquires it once this process
	// closes it, which it could not if it had inherited the descriptor.
	probe := startHelper(t, helperProbe, store.Root)
	requireLine(t, probe, "ready")
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	probe.Release(t)
	requireLine(t, probe, "acquired")
	for _, helper := range []*testchild.Child{control, probe} {
		if code := helper.Finish(t); code != 0 {
			t.Fatalf("probe exited %d", code)
		}
	}
}

func TestStoreWorksWhileAnotherProcessHoldsTheLease(t *testing.T) {
	store := stateStore(t)
	migrated(t, store)
	holder := startHelper(t, helperHold, store.Root)
	requireLine(t, holder, "held")
	if _, err := AcquireOwnerLease(testContext(t), store); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("lease held elsewhere err=%v, want task.ErrLockBusy", err)
	}
	scheduler := openScheduler(t, store)
	if err := scheduler.CreateSchedule(testContext(t), testDefinition()); err != nil {
		t.Fatalf("create while the lease is held elsewhere: %v", err)
	}
	pause := contract.StateChange{ScheduleID: testScheduleID, From: contract.StateEnabled, To: contract.StatePaused}
	if err := scheduler.ChangeState(testContext(t), pause); err != nil {
		t.Fatalf("change state while the lease is held elsewhere: %v", err)
	}
	if code := holder.Finish(t); code != 0 {
		t.Fatalf("holder exited %d", code)
	}
	acquire(t, store)
}

func TestConcurrentMigrationsLeaveOneCurrentDatabase(t *testing.T) {
	store := stateStore(t)
	helpers := []*testchild.Child{startHelper(t, helperMigrate, store.Root), startHelper(t, helperMigrate, store.Root)}
	for _, helper := range helpers {
		requireLine(t, helper, "ready")
	}
	for _, helper := range helpers {
		helper.Release(t)
	}
	acquired := 0
	for _, helper := range helpers {
		switch line := helper.Line(t); line {
		case "acquired":
			acquired++
		case "busy":
		default:
			t.Fatalf("migrate helper said %q", line)
		}
		if code := helper.Finish(t); code != 0 {
			t.Fatalf("migrate helper exited %d", code)
		}
	}
	if acquired == 0 {
		t.Fatal("no helper migrated")
	}
	// A loser retries under the lease; migration of a current database is a
	// no-op that keeps its data.
	scheduler := openScheduler(t, store)
	if err := scheduler.CreateSchedule(testContext(t), testDefinition()); err != nil {
		t.Fatal(err)
	}
	migrated(t, store)
	if _, err := scheduler.GetSchedule(testContext(t), testScheduleID); err != nil {
		t.Fatalf("schedule lost by a retried migration: %v", err)
	}
}

func TestSchedulerLeavesTaskStateUntouched(t *testing.T) {
	store := stateStore(t)
	if err := os.WriteFile(filepath.Join(store.Root, "operator-note.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, store.Root)
	migrated(t, store)
	scheduler := openScheduler(t, store)
	if err := scheduler.CreateSchedule(testContext(t), testDefinition()); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Close(); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, store.Root); !reflect.DeepEqual(after, before) {
		t.Fatalf("state outside %s changed:\nbefore %v\nafter  %v", sqlitestore.DirectoryName, before, after)
	}
}

// snapshot records the mode and content digest of every entry below root
// except the scheduler directory.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	entries := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == sqlitestore.DirectoryName {
			return filepath.SkipDir
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		fact := info.Mode().String()
		if info.Mode().IsRegular() {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			sum := sha256.Sum256(data)
			fact += " " + hex.EncodeToString(sum[:])
		}
		entries[relative] = fact
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func requireLine(t *testing.T, helper *testchild.Child, want string) {
	t.Helper()
	if line := helper.Line(t); strings.TrimSpace(line) != want {
		t.Fatalf("helper said %q, want %q", line, want)
	}
}

var errUnlock = errors.New("injected unlock failure")

// failingLock fails its first Close, as taskdir.LockFile.Close does when the
// unlock fails while it keeps the descriptor.
type failingLock struct{ closes int }

func (f *failingLock) Close() error {
	f.closes++
	if f.closes == 1 {
		return errUnlock
	}
	return nil
}

func TestFailedLeaseCloseKeepsTheHandleForRetry(t *testing.T) {
	store := stateStore(t)
	held := &failingLock{}
	lease := &OwnerLease{root: store.Root, rootID: store.RootID, lock: held}
	if err := lease.Close(); !errors.Is(err, errUnlock) {
		t.Fatalf("first close err=%v, want the unlock failure", err)
	}
	if err := lease.Migrate(testContext(t)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("migrate after a failed close err=%v, want os.ErrClosed", err)
	}
	if _, err := os.Lstat(sqlitestore.Directory(store.Root)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("migrate after a failed close touched the state: %v", err)
	}
	if err := lease.Close(); err != nil || held.closes != 2 {
		t.Fatalf("retried close err=%v after %d closes, want success on the same handle", err, held.closes)
	}
	if err := lease.Close(); err != nil || held.closes != 2 {
		t.Fatalf("close after release err=%v after %d closes, want a no-op", err, held.closes)
	}
}

func TestMigrationUnderTheLeasePreservesStoredData(t *testing.T) {
	store := stateStore(t)
	migrated(t, store)
	scheduler := openScheduler(t, store)
	ctx := testContext(t)
	if err := scheduler.CreateSchedule(ctx, testDefinition()); err != nil {
		t.Fatal(err)
	}
	claim := contract.Claim{
		ScheduleID: testScheduleID, ExpectedCursor: time.Unix(1_000, 0).UTC(), Due: time.Unix(2_000, 0).UTC(),
		TaskID: testTaskID, ArmedAt: time.Unix(2_000, 0).UTC(),
	}
	if result, err := scheduler.ClaimAndArm(ctx, claim); err != nil || !result.AuthorizesInvocation() {
		t.Fatalf("claim = %+v, %v", result, err)
	}
	observation := contract.OccurrenceObservation{TaskID: testTaskID, Observation: contract.Observation{
		ObservedAt: time.Unix(2_050, 0).UTC(), Admission: task.AdmissionAdmitted, Liveness: task.LivenessRunning, Publication: task.PublicationPending,
	}}
	release := contract.OccurrenceRelease{TaskID: testTaskID, Release: contract.Release{ReleasedAt: time.Unix(2_100, 0).UTC(), Kind: contract.ReleaseNotCreated, Ref: "admission_refused"}}
	skip := contract.Skip{
		ScheduleID: testScheduleID, ExpectedCursor: time.Unix(2_000, 0).UTC(),
		Span: contract.SkipSpan{From: time.Unix(2_000, 0).UTC(), To: time.Unix(3_000, 0).UTC(), Reason: contract.SkipMissedCapacity},
	}
	if err := errors.Join(scheduler.RecordObservation(ctx, observation), scheduler.Release(ctx, release), scheduler.RecordSkip(ctx, skip)); err != nil {
		t.Fatal(err)
	}
	before := facadeState(t, scheduler)
	lease := acquire(t, store)
	for range 2 {
		if err := lease.Migrate(ctx); err != nil {
			t.Fatalf("migrate a current database: %v", err)
		}
	}
	if after := facadeState(t, scheduler); !reflect.DeepEqual(after, before) {
		t.Fatalf("migration changed stored data:\nbefore %+v\nafter  %+v", before, after)
	}
}

type facadeReads struct {
	schedule   contract.Schedule
	occurrence contract.Occurrence
	history    contract.HistoryPageResult
}

func facadeState(t *testing.T, scheduler contract.Store) facadeReads {
	t.Helper()
	ctx := testContext(t)
	var reads facadeReads
	var scheduleErr, occurrenceErr, historyErr error
	reads.schedule, scheduleErr = scheduler.GetSchedule(ctx, testScheduleID)
	reads.occurrence, occurrenceErr = scheduler.GetOccurrence(ctx, testTaskID)
	reads.history, historyErr = scheduler.History(ctx, contract.HistoryPage{ScheduleID: testScheduleID, Limit: contract.MaxPageLimit})
	if err := errors.Join(scheduleErr, occurrenceErr, historyErr); err != nil {
		t.Fatal(err)
	}
	if reads.occurrence.Release == nil || reads.occurrence.Observation == nil || len(reads.history.Entries) != 2 {
		t.Fatalf("fixture lacks a released, observed occurrence and a skip span: %+v", reads)
	}
	return reads
}
