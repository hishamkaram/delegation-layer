package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/internal/sqlitestore"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/internal/testchild"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"modernc.org/sqlite"
)

// CLI compatibility fixtures. The hermetic ordinary-CLI acceptance case runs
// this test binary in these modes. Each acquires the real owner lease,
// prepares one scheduler state, reports ready only once that authority is
// held, holds it until its stdin barrier closes or its deadline ends, and then
// releases everything it took.
const (
	fixtureMissing = "cli-fixture-missing"
	fixtureCorrupt = "cli-fixture-corrupt"
	fixtureNewer   = "cli-fixture-newer"
	fixtureLocked  = "cli-fixture-locked"
	// probeStore opens the scheduler store once and reports its class.
	probeStore = "cli-probe-store"
	// fixtureDeadline bounds a hold across one complete ordinary CLI round.
	fixtureDeadline = 120 * time.Second
	// unsupportedVersion is a user_version this build never writes.
	unsupportedVersion = 3
)

// corruptDatabase is private non-SQLite bytes at the database path.
var corruptDatabase = []byte("scheduler compatibility fixture: not a SQLite database\n")

func isCLIFixture(mode string) bool {
	switch mode {
	case fixtureMissing, fixtureCorrupt, fixtureNewer, fixtureLocked:
		return true
	default:
		return false
	}
}

func runCLIFixture(ctx context.Context, mode string, store *taskdir.Store) error {
	lease, err := AcquireOwnerLease(ctx, store)
	if err != nil {
		return err
	}
	return errors.Join(holdState(ctx, mode, lease, store.Root), lease.Close())
}

func holdState(ctx context.Context, mode string, lease *OwnerLease, root string) error {
	switch mode {
	case fixtureCorrupt:
		if err := writePrivateFile(sqlitestore.DatabasePath(root), corruptDatabase); err != nil {
			return err
		}
	case fixtureNewer:
		if err := lease.Migrate(ctx); err != nil {
			return err
		}
		if err := rawStatements(ctx, root, fmt.Sprintf("PRAGMA user_version = %d", unsupportedVersion)); err != nil {
			return err
		}
	case fixtureLocked:
		if err := lease.Migrate(ctx); err != nil {
			return err
		}
		return holdExclusive(ctx, root)
	}
	return awaitFixtureRelease(ctx)
}

func awaitFixtureRelease(ctx context.Context) error {
	return testchild.AwaitRelease(ctx, "ready")
}

func writePrivateFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	return errors.Join(writeErr, file.Sync(), file.Close())
}

// rawConnection is one independent physical connection that fails at once,
// instead of waiting, when another connection holds a conflicting lock.
func rawConnection(ctx context.Context, root string) (*sql.DB, *sql.Conn, error) {
	location := url.URL{Scheme: "file", Path: filepath.ToSlash(sqlitestore.DatabasePath(root)), RawQuery: "mode=rw&_pragma=busy_timeout%280%29"}
	connector, err := sqlite.NewConnector(location.String())
	if err != nil {
		return nil, nil, err
	}
	db := sql.OpenDB(connector)
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, errors.Join(err, db.Close())
	}
	return db, conn, nil
}

func rawStatements(ctx context.Context, root string, statements ...string) error {
	db, conn, err := rawConnection(ctx, root)
	if err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err = conn.ExecContext(ctx, statement); err != nil {
			break
		}
	}
	return errors.Join(err, conn.Close(), db.Close())
}

// holdExclusive holds a real SQLite EXCLUSIVE transaction in this process
// until the barrier closes, then rolls it back.
func holdExclusive(ctx context.Context, root string) error {
	db, conn, err := rawConnection(ctx, root)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "BEGIN EXCLUSIVE")
	if err == nil {
		err = awaitFixtureRelease(ctx)
		// The rollback keeps the hold's values but not its cancellation,
		// because the hold may have ended at its deadline.
		_, rollbackErr := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		err = errors.Join(err, rollbackErr)
	}
	return errors.Join(err, conn.Close(), db.Close())
}

// probeStoreClass opens the scheduler store once and reports one class:
// open, missing, unsupported, or busy.
func probeStoreClass(ctx context.Context, store *taskdir.Store) error {
	opened, err := Open(ctx, store)
	switch {
	case err == nil:
		return errors.Join(report("open"), opened.Close())
	case errors.Is(err, fs.ErrNotExist):
		return report("missing")
	case errors.Is(err, task.ErrUnsupportedSchema):
		return report("unsupported")
	case errors.Is(err, task.ErrLockBusy):
		return report("busy")
	default:
		return err
	}
}
