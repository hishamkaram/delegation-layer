package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"modernc.org/sqlite"
)

const (
	testRootID     = "0123456789abcdef0123456789abcdef"
	otherRootID    = "fedcba9876543210fedcba9876543210"
	testScheduleID = "11111111111111111111111111111111"
	otherSchedule  = "22222222222222222222222222222222"
	testTaskID     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	otherTaskID    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testDeadline   = 30 * time.Second
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == helperFlag {
		os.Exit(runHelper(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func at(seconds int64) time.Time { return time.Unix(seconds, 0).UTC() }

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	t.Cleanup(cancel)
	return ctx
}

// newRoot returns a native temporary state root with the private scheduler
// directory the owner lease would create.
func newRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(Directory(root), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func initializedStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := newRoot(t)
	if err := Initialize(testContext(t), root, testRootID); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return openStore(t, root), root
}

func openStore(t *testing.T, root string) *Store {
	t.Helper()
	store, err := Open(testContext(t), root, testRootID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}

func testDefinition(scheduleID string) contract.Definition {
	return contract.Definition{
		DefinitionHeader: contract.DefinitionHeader{
			ScheduleID: scheduleID,
			CreatedAt:  at(1_000),
			Cron:       "0 9 * * 1-5",
			Timezone:   "Europe/Berlin",
			Request: contract.Request{
				Auto:         true,
				CanonicalCwd: "/work/repository",
				Config:       task.TaskConfig{Permission: "read-only", ReviewTarget: &task.ReviewTarget{Kind: task.ReviewTargetBase, Value: "main"}},
			},
		},
		Brief: []byte("summarize open issues"),
	}
}

func createSchedule(t *testing.T, store *Store, scheduleID string) {
	t.Helper()
	if err := store.CreateSchedule(testContext(t), testDefinition(scheduleID)); err != nil {
		t.Fatalf("create schedule %s: %v", scheduleID, err)
	}
}

func testClaim(scheduleID, taskID string, cursor, due int64) contract.Claim {
	return contract.Claim{ScheduleID: scheduleID, ExpectedCursor: at(cursor), Due: at(due), TaskID: taskID, ArmedAt: at(due)}
}

// rawURI opens path with only a busy timeout and default deferred
// transactions, as an independent process with no scheduler settings would.
func rawURI(path string) string {
	location := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=rw&_pragma=busy_timeout%285000%29"}
	return location.String()
}

// rawConn returns one independent physical connection to path.
func rawConn(t *testing.T, path string) *sql.Conn {
	t.Helper()
	connector, err := sqlite.NewConnector(rawURI(path))
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
			t.Errorf("close raw connection: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Errorf("close raw database: %v", err)
		}
	})
	return conn
}

func rawExec(t *testing.T, conn *sql.Conn, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := conn.ExecContext(testContext(t), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func rawInt(t *testing.T, conn *sql.Conn, query string) int64 {
	t.Helper()
	var value int64
	if err := conn.QueryRowContext(testContext(t), query).Scan(&value); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return value
}

func rawText(t *testing.T, conn *sql.Conn, query string) string {
	t.Helper()
	var value string
	if err := conn.QueryRowContext(testContext(t), query).Scan(&value); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return value
}

func storeConnector(t *testing.T, path string) *faultConnector {
	t.Helper()
	base, err := sqlite.NewConnector(databaseURI(path, runtime.GOOS))
	if err != nil {
		t.Fatal(err)
	}
	return &faultConnector{base: base}
}
