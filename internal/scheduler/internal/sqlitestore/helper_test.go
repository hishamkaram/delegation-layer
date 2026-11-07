package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/internal/testchild"
	"modernc.org/sqlite"
)

// Helper processes re-execute this test binary through testchild. Each is
// finite: it reports readiness, waits for its stdin to close as the start
// barrier or its own deadline, acts once, and exits on its own. No signal is
// ever sent.
const (
	helperFlag      = "--scheduler-sqlitestore-helper"
	helperClaim     = "claim"
	helperInterrupt = "interrupt"
	// interruptExit is the deliberate exit with an uncommitted transaction.
	interruptExit = 23
	// A 16-page cache cannot hold about 2 MiB of uncommitted blob pages, so
	// SQLite must spill dirty pages into the database before commit.
	spillCachePages = 16
	spillRows       = 512
	spillBlobBytes  = 4096
)

func runHelper(args []string) int {
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	var err error
	switch {
	case len(args) == 7 && args[0] == helperClaim:
		err = helperClaimOnce(ctx, args[1:])
	case len(args) == 3 && args[0] == helperInterrupt:
		if err = helperInterruptTransaction(ctx, args[1], args[2]); err == nil {
			// Exit with the transaction and connection still open.
			return interruptExit
		}
	default:
		err = fmt.Errorf("unknown helper arguments %q", args)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	return 0
}

// helperClaimOnce opens the production Store, waits for the barrier, and
// prints the claim outcome.
func helperClaimOnce(ctx context.Context, args []string) (resultErr error) {
	root, rootID, scheduleID, taskID := args[0], args[1], args[2], args[5]
	cursor, cursorErr := strconv.ParseInt(args[3], 10, 64)
	due, dueErr := strconv.ParseInt(args[4], 10, 64)
	if err := errors.Join(cursorErr, dueErr); err != nil {
		return err
	}
	store, err := Open(ctx, root, rootID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	if err = testchild.AwaitRelease(ctx, "ready"); err != nil {
		return err
	}
	result, claimErr := store.ClaimAndArm(ctx, testClaim(scheduleID, taskID, cursor, due))
	conflict := errors.Is(claimErr, contract.ErrConflict)
	if claimErr != nil && !conflict {
		return claimErr
	}
	_, err = fmt.Fprintf(os.Stdout, "outcome=%d task=%s conflict=%t\n", result.Outcome, result.TaskID, conflict)
	return err
}

// helperInterruptTransaction writes enough uncommitted pages to spill into the
// database file and confirms the hot journal exists; the caller then exits
// without commit or close. mode "fresh" replays the version 1 DDL on the empty
// file as initialization does; mode "populated" changes an existing schedule's
// cursor and history first.
func helperInterruptTransaction(ctx context.Context, path, mode string) error {
	connector, err := sqlite.NewConnector(rawURI(path))
	if err != nil {
		return err
	}
	conn, err := sql.OpenDB(connector).Conn(ctx)
	if err != nil {
		return err
	}
	statements := []string{fmt.Sprintf("PRAGMA cache_size = %d", spillCachePages), "BEGIN IMMEDIATE"}
	if mode == "fresh" {
		for _, object := range schemaObjects() {
			statements = append(statements, object.sql)
		}
	} else {
		statements = append(statements,
			"INSERT INTO skip_spans (schedule_id, from_unix_nanos, to_unix_nanos, reason) "+
				"SELECT schedule_id, cursor_unix_nanos, cursor_unix_nanos + 1, 'missed_unavailable' FROM schedules",
			"UPDATE schedules SET cursor_unix_nanos = cursor_unix_nanos + 1")
	}
	statements = append(statements, "CREATE TABLE spill (payload BLOB) STRICT")
	for _, statement := range statements {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("%s: %w", statement, err)
		}
	}
	payload := make([]byte, spillBlobBytes)
	for range spillRows {
		if _, err := conn.ExecContext(ctx, "INSERT INTO spill (payload) VALUES (?)", payload); err != nil {
			return err
		}
	}
	return requireHotJournal(path)
}

func requireHotJournal(path string) error {
	journal, err := os.Stat(path + "-journal")
	if err != nil {
		return fmt.Errorf("hot journal missing: %w", err)
	}
	database, err := os.Stat(path)
	if err != nil {
		return err
	}
	if journal.Size() == 0 || database.Size() == 0 {
		return fmt.Errorf("no spill: journal %d bytes, database %d bytes", journal.Size(), database.Size())
	}
	return nil
}

// startHelper launches this test binary as one helper child; the shared
// testchild owner joins it at cleanup.
func startHelper(t *testing.T, args ...string) *testchild.Child {
	t.Helper()
	return testchild.Launch(t, append([]string{helperFlag}, args...)...)
}
