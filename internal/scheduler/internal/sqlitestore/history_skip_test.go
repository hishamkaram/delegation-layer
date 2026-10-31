package sqlitestore

import (
	"context"
	"database/sql/driver"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

func TestRecordSkipAdvancesEnabledSchedule(t *testing.T) {
	store, _ := initializedStore(t)
	createSchedule(t, store, testScheduleID)
	seed := historySkip(1_000, 2_000)
	if err := store.RecordSkip(testContext(t), seed); err != nil {
		t.Fatal(err)
	}
	skip := historySkip(2_000, 3_000)
	if err := store.RecordSkip(testContext(t), skip); err != nil {
		t.Fatalf("enabled skip: %v", err)
	}
	requireSchedule(t, store, contract.StateEnabled, 3_000)
	requireSkipHistory(t, store, skip.Span, seed.Span)
}

func TestRecordSkipRequiresEnabledSchedule(t *testing.T) {
	for _, state := range []contract.State{contract.StatePaused, contract.StateDeleted} {
		t.Run(string(state), func(t *testing.T) {
			store, _ := initializedStore(t)
			createSchedule(t, store, testScheduleID)
			seed := historySkip(1_000, 2_000)
			if err := store.RecordSkip(testContext(t), seed); err != nil {
				t.Fatal(err)
			}
			change := contract.StateChange{ScheduleID: testScheduleID, From: contract.StateEnabled, To: state}
			if err := store.ChangeState(testContext(t), change); err != nil {
				t.Fatal(err)
			}
			skip := historySkip(2_000, 3_000)
			err := store.RecordSkip(testContext(t), skip)
			if !errors.Is(err, contract.ErrConflict) {
				t.Errorf("%s skip err=%v, want ErrConflict", state, err)
			}
			requireSchedule(t, store, state, 2_000)
			requireSkipHistory(t, store, seed.Span)
		})
	}
}

func TestRecordSkipRechecksStateAndCursorAtTransactionBegin(t *testing.T) {
	for _, state := range []contract.State{contract.StatePaused, contract.StateDeleted, contract.StateEnabled} {
		name := string(state)
		if state == contract.StateEnabled {
			name = "cursor advanced"
		}
		t.Run(name, func(t *testing.T) {
			writer, root := initializedStore(t)
			createSchedule(t, writer, testScheduleID)
			seed := historySkip(1_000, 2_000)
			if err := writer.RecordSkip(testContext(t), seed); err != nil {
				t.Fatal(err)
			}
			store, gate := gatedSkipStore(t, root)
			finish := skipBeforeBegin(t, store, gate, historySkip(2_000, 4_000))
			cursor, spans := int64(2_000), []contract.SkipSpan{seed.Span}
			if state == contract.StateEnabled {
				winner := historySkip(2_000, 3_000)
				if err := writer.RecordSkip(testContext(t), winner); err != nil {
					t.Fatal(err)
				}
				cursor, spans = 3_000, []contract.SkipSpan{winner.Span, seed.Span}
			} else {
				change := contract.StateChange{ScheduleID: testScheduleID, From: contract.StateEnabled, To: state}
				if err := writer.ChangeState(testContext(t), change); err != nil {
					t.Fatal(err)
				}
			}
			if err := finish(); !errors.Is(err, contract.ErrConflict) {
				t.Errorf("skip after concurrent %s err=%v, want ErrConflict", name, err)
			}
			requireSchedule(t, writer, state, cursor)
			requireSkipHistory(t, writer, spans...)
		})
	}
}

func historySkip(from, to int64) contract.Skip {
	return contract.Skip{
		ScheduleID: testScheduleID, ExpectedCursor: at(from),
		Span: contract.SkipSpan{From: at(from), To: at(to), Reason: contract.SkipMissedCapacity},
	}
}

func requireSkipHistory(t *testing.T, store *Store, want ...contract.SkipSpan) {
	t.Helper()
	page, err := store.History(testContext(t), contract.HistoryPage{ScheduleID: testScheduleID, Limit: contract.MaxPageLimit})
	if err != nil || page.More || len(page.Entries) != len(want) {
		t.Fatalf("history %+v, %v; want %d skip spans", page, err, len(want))
	}
	for i, span := range want {
		got, isSkip := page.Entries[i].Skip()
		if !isSkip || !got.From.Equal(span.From) || !got.To.Equal(span.To) || got.Reason != span.Reason {
			t.Errorf("history entry %d = %+v, isSkip=%t; want %+v", i, got, isSkip, span)
		}
	}
}

// skipBeginGate holds one operation at the driver's transaction boundary,
// before BEGIN IMMEDIATE, so another Store can commit the competing write.
type skipBeginGate struct {
	armed   atomic.Bool
	entered chan struct{}
	proceed chan struct{}
}

type skipGateConnector struct {
	*faultConnector
	gate *skipBeginGate
}

func (c *skipGateConnector) Connect(ctx context.Context) (driver.Conn, error) {
	real, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	wrapped, err := wrapConn(real, c.faultConnector)
	if err != nil {
		return nil, errors.Join(err, real.Close())
	}
	return &skipGateConn{faultConn: wrapped, gate: c.gate}, nil
}

type skipGateConn struct {
	*faultConn
	gate *skipBeginGate
}

func (c *skipGateConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.gate.armed.CompareAndSwap(true, false) {
		close(c.gate.entered)
		select {
		case <-c.gate.proceed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return c.faultConn.BeginTx(ctx, opts)
}

func gatedSkipStore(t *testing.T, root string) (*Store, *skipBeginGate) {
	t.Helper()
	gate := &skipBeginGate{entered: make(chan struct{}), proceed: make(chan struct{})}
	connector := &skipGateConnector{faultConnector: storeConnector(t, DatabasePath(root)), gate: gate}
	store, err := openWithConnector(testContext(t), connector, DatabasePath(root), testRootID, runtime.GOOS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close gated skip store: %v", err)
		}
	})
	return store, gate
}

// skipBeforeBegin owns the pending RecordSkip goroutine and releases and joins
// it on every path. It returns only once the driver's BeginTx has been entered.
func skipBeforeBegin(t *testing.T, store *Store, gate *skipBeginGate, skip contract.Skip) func() error {
	t.Helper()
	ctx, cancel := context.WithCancel(testContext(t))
	release := sync.OnceFunc(func() { close(gate.proceed) })
	done := make(chan struct{})
	var result error
	gate.armed.Store(true)
	go func() {
		defer close(done)
		result = store.RecordSkip(ctx, skip)
	}()
	t.Cleanup(func() {
		cancel()
		release()
		<-done
	})
	select {
	case <-gate.entered:
	case <-done:
		t.Fatalf("skip ended before BeginTx: %v", result)
	case <-ctx.Done():
		t.Fatalf("waiting for skip BeginTx: %v", ctx.Err())
	}
	return func() error {
		release()
		<-done
		return result
	}
}
