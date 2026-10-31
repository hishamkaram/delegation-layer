package sqlitestore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// faultStore opens an initialized database through the fault connector.
func faultStore(t *testing.T) (*Store, *faultConnector, string) {
	t.Helper()
	root := newRoot(t)
	if err := Initialize(testContext(t), root, testRootID); err != nil {
		t.Fatal(err)
	}
	connector := storeConnector(t, DatabasePath(root))
	store, err := openWithConnector(testContext(t), connector, DatabasePath(root), testRootID, runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store, connector, root
}

// requireOpens checks how many physical connections the store has opened.
func requireOpens(t *testing.T, connector *faultConnector, want int64) {
	t.Helper()
	if got := connector.opens.Load(); got != want {
		t.Fatalf("%d physical connections opened, want %d", got, want)
	}
}

// requireWritable proves no leftover lock by taking the write lock from an
// independent connection.
func requireWritable(t *testing.T, root string) {
	t.Helper()
	conn := rawConn(t, DatabasePath(root))
	rawExec(t, conn, "BEGIN IMMEDIATE", "ROLLBACK")
	closeRaw(t, conn)
}

func scheduleCount(t *testing.T, root string) int64 {
	t.Helper()
	conn := rawConn(t, DatabasePath(root))
	defer closeRaw(t, conn)
	return rawInt(t, conn, "SELECT count(*) FROM schedules")
}

// tempMarker reports whether the current physical connection still has the
// TEMP table an earlier operation created on it.
func (s *Store) tempMarker(ctx context.Context) (bool, error) {
	var present bool
	err := s.transact(ctx, s.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		var count int64
		present = false
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_temp_schema WHERE name = 'marker'").Scan(&count); err != nil {
			return err
		}
		present = count == 1
		return nil
	})
	return present, err
}

func createMarker(t *testing.T, store *Store) {
	t.Helper()
	err := store.transact(testContext(t), store.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "CREATE TEMP TABLE marker (x INTEGER)")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func requireMarker(t *testing.T, store *Store, want bool) {
	t.Helper()
	present, err := store.tempMarker(testContext(t))
	if err != nil || present != want {
		t.Fatalf("TEMP marker present=%t err=%v, want %t", present, err, want)
	}
}

func TestCommitBusyFromAnIndependentReaderIsUnknownAndRetires(t *testing.T) {
	store, connector, root := faultStore(t)
	reader := rawConn(t, DatabasePath(root))
	rawExec(t, reader, "BEGIN")
	if count := rawInt(t, reader, "SELECT count(*) FROM schedules"); count != 0 {
		t.Fatal(count)
	}
	err := store.CreateSchedule(testContext(t), testDefinition(testScheduleID))
	if !errors.Is(err, errCommitOutcomeUnknown) || !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("commit behind a reader err=%v, want unknown busy commit", err)
	}
	rawExec(t, reader, "COMMIT")
	closeRaw(t, reader)
	requireWritable(t, root)
	createSchedule(t, store, testScheduleID)
	requireOpens(t, connector, 2)
}

func TestInjectedCommitFaultsAreUnknownAndRetire(t *testing.T) {
	cases := map[string]struct {
		fault   finalizationFault
		applied int64
	}{
		"before the real commit": {fault: faultCommitBeforeReal, applied: 0},
		"after the real commit":  {fault: faultCommitAfterReal, applied: 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store, connector, root := faultStore(t)
			createMarker(t, store)
			connector.inject(tc.fault)
			err := store.CreateSchedule(testContext(t), testDefinition(testScheduleID))
			if !errors.Is(err, errCommitOutcomeUnknown) || !errors.Is(err, errInjected) {
				t.Fatalf("err=%v, want unknown commit outcome", err)
			}
			requireWritable(t, root)
			if count := scheduleCount(t, root); count != tc.applied {
				t.Fatalf("%d schedules after the fault, want %d", count, tc.applied)
			}
			requireMarker(t, store, false)
			requireOpens(t, connector, 2)
		})
	}
}

func TestClaimWithUnknownCommitNeverGrants(t *testing.T) {
	store, connector, _ := faultStore(t)
	createSchedule(t, store, testScheduleID)
	connector.inject(faultCommitAfterReal)
	result, err := store.ClaimAndArm(testContext(t), testClaim(testScheduleID, testTaskID, 1_000, 2_000))
	if !errors.Is(err, errCommitOutcomeUnknown) || result != (contract.ClaimResult{}) || result.AuthorizesInvocation() {
		t.Fatalf("claim = %+v, %v; want zero ClaimUnknown", result, err)
	}
	existing := contract.ClaimResult{Outcome: contract.ClaimExisting, TaskID: testTaskID}
	if again, err := store.ClaimAndArm(testContext(t), testClaim(testScheduleID, testTaskID, 1_000, 2_000)); err != nil || again != existing {
		t.Fatalf("re-read claim = %+v, %v; want %+v", again, err, existing)
	}
}

func TestRollbackFailureIsReportedAndRetires(t *testing.T) {
	store, connector, root := faultStore(t)
	createSchedule(t, store, testScheduleID)
	createMarker(t, store)
	connector.inject(faultRollback)
	err := store.CreateSchedule(testContext(t), testDefinition(testScheduleID))
	if !errors.Is(err, contract.ErrConflict) || !errors.Is(err, errInjected) {
		t.Fatalf("err=%v, want the body conflict joined with the rollback fault", err)
	}
	requireWritable(t, root)
	requireMarker(t, store, false)
	requireOpens(t, connector, 2)
}

func TestCleanOutcomesReuseTheConnection(t *testing.T) {
	store, connector, _ := faultStore(t)
	createMarker(t, store)
	createSchedule(t, store, testScheduleID)
	if err := store.CreateSchedule(testContext(t), testDefinition(testScheduleID)); !errors.Is(err, contract.ErrConflict) {
		t.Fatalf("conflict err=%v", err)
	}
	if _, err := store.GetSchedule(testContext(t), otherSchedule); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing err=%v", err)
	}
	requireMarker(t, store, true)
	requireOpens(t, connector, 1)
}

func TestCancellationDuringTheBodyRetiresTheConnection(t *testing.T) {
	store, connector, root := faultStore(t)
	createMarker(t, store)
	ctx, cancel := context.WithCancel(testContext(t))
	err := store.transact(ctx, store.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE scheduler_metadata SET singleton = singleton WHERE 0"); err != nil {
			return err
		}
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	requireWritable(t, root)
	requireMarker(t, store, false)
	requireOpens(t, connector, 2)
}

func TestReconnectedConnectionPassesEveryGuard(t *testing.T) {
	store, connector, _ := faultStore(t)
	connector.inject(faultCommitBeforeReal)
	if err := store.CreateSchedule(testContext(t), testDefinition(testScheduleID)); !errors.Is(err, errCommitOutcomeUnknown) {
		t.Fatalf("err=%v", err)
	}
	err := store.transact(testContext(t), store.currentGuard, func(ctx context.Context, tx *sql.Tx) error {
		var mode string
		return tx.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode)
	})
	if err != nil {
		t.Fatalf("guarded operation on the new connection: %v", err)
	}
	createSchedule(t, store, testScheduleID)
	requireOpens(t, connector, 2)
}

// heldTransaction is one operation a test holds open inside its body. The
// test that starts it owns its goroutine, release gate, and result.
type heldTransaction struct {
	release func()
	// done closes after err is written, so err is read only after done.
	done chan struct{}
	err  error
}

// holdTransaction starts one guarded operation whose body waits for release
// or its context. It returns once the body is entered, or with the
// operation's error when the operation ends before its body. Cleanup cancels,
// releases, and joins the operation on every path, including a failed
// assertion.
func holdTransaction(t *testing.T, store *Store, guard txStep) (*heldTransaction, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(testContext(t))
	entered, released := make(chan struct{}), make(chan struct{})
	held := &heldTransaction{release: sync.OnceFunc(func() { close(released) }), done: make(chan struct{})}
	go func() {
		defer close(held.done)
		held.err = store.transact(ctx, guard, func(ctx context.Context, _ *sql.Tx) error {
			close(entered)
			select {
			case <-released:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	t.Cleanup(func() {
		cancel()
		held.release()
		<-held.done
	})
	select {
	case <-entered:
		return held, nil
	case <-held.done:
		return nil, fmt.Errorf("held operation ended before its body: %w", held.err)
	}
}

// finish releases the held body and returns the operation's result.
func (h *heldTransaction) finish() error {
	h.release()
	<-h.done
	return h.err
}

func TestSecondOperationWaitsAndNeverNestsATransaction(t *testing.T) {
	store, connector, _ := faultStore(t)
	held, err := holdTransaction(t, store, store.currentGuard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(testContext(t), 100*time.Millisecond)
	defer cancel()
	if err := store.CreateSchedule(ctx, testDefinition(testScheduleID)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second operation err=%v, want its own deadline", err)
	}
	if err := held.finish(); err != nil {
		t.Fatalf("first operation: %v", err)
	}
	createSchedule(t, store, testScheduleID)
	requireOpens(t, connector, 1)
}

func TestHeldTransactionReportsAFailureBeforeItsBody(t *testing.T) {
	store, connector, _ := faultStore(t)
	errGuard := errors.New("guard refused the held operation")
	_, err := holdTransaction(t, store, func(context.Context, *sql.Tx) error { return errGuard })
	if !errors.Is(err, errGuard) {
		t.Fatalf("held operation err=%v, want the guard failure", err)
	}
	createSchedule(t, store, testScheduleID)
	requireOpens(t, connector, 1)
}

func TestHeldTransactionCleanupCancelsAndJoinsAnAbandonedOperation(t *testing.T) {
	store, connector, _ := faultStore(t)
	var held *heldTransaction
	if !t.Run("abandoned before finish", func(t *testing.T) {
		var err error
		if held, err = holdTransaction(t, store, store.currentGuard); err != nil {
			t.Fatal(err)
		}
	}) {
		return
	}
	select {
	case <-held.done:
	default:
		t.Fatal("subtest cleanup returned with the held operation still running")
	}
	createSchedule(t, store, testScheduleID)
	// The cleanup canceled the operation before it ended, so transact retired
	// its connection.
	requireOpens(t, connector, 2)
}

func TestFaultConnectionKeepsTheDriverInterfaceSet(t *testing.T) {
	_, root := initializedStore(t)
	connector := storeConnector(t, DatabasePath(root))
	real, err := connector.base.Connect(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := connector.Connect(testContext(t))
	if err != nil {
		t.Fatal(errors.Join(err, real.Close()))
	}
	t.Cleanup(func() {
		if err := errors.Join(real.Close(), wrapped.Close()); err != nil {
			t.Error(err)
		}
	})
	// database/sql never uses the legacy Exec and Query methods when
	// ExecerContext and QueryerContext exist, so the wrapper omits them.
	want := optionalInterfaces(real)
	if !want["legacy Exec"] || !want["legacy Query"] || want["NamedValueChecker"] {
		t.Fatalf("real connection interface set changed: %v", want)
	}
	want["legacy Exec"], want["legacy Query"] = false, false
	if got := optionalInterfaces(wrapped); !reflect.DeepEqual(got, want) {
		t.Fatalf("wrapper interfaces %v, real %v", got, want)
	}
}

// The legacy method sets, declared locally to detect them without naming
// the deprecated driver interfaces.
type (
	legacyExecer interface {
		Exec(query string, args []driver.Value) (driver.Result, error)
	}
	legacyQueryer interface {
		Query(query string, args []driver.Value) (driver.Rows, error)
	}
)

func optionalInterfaces(conn driver.Conn) map[string]bool {
	present := map[string]bool{}
	_, present["ConnBeginTx"] = conn.(driver.ConnBeginTx)
	_, present["ConnPrepareContext"] = conn.(driver.ConnPrepareContext)
	_, present["ExecerContext"] = conn.(driver.ExecerContext)
	_, present["QueryerContext"] = conn.(driver.QueryerContext)
	_, present["Pinger"] = conn.(driver.Pinger)
	_, present["SessionResetter"] = conn.(driver.SessionResetter)
	_, present["Validator"] = conn.(driver.Validator)
	_, present["NamedValueChecker"] = conn.(driver.NamedValueChecker)
	_, present["legacy Exec"] = conn.(legacyExecer)
	_, present["legacy Query"] = conn.(legacyQueryer)
	return present
}
