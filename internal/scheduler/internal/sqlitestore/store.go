// Package sqlitestore is the sealed SQLite implementation of the scheduler's
// contract.Store. It is the only owner and writer of the scheduler database;
// each Store method is one guarded immediate transaction on one reserved
// connection, and no SQL handle or transaction leaves the package.
package sqlitestore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"golang.org/x/sys/unix"
	"modernc.org/sqlite"
)

// Store is one open scheduler database bound to one state root.
type Store struct {
	db     *sql.DB
	path   string
	rootID string
	goos   string
}

var _ contract.Store = (*Store)(nil)

// Open opens the current-version database of root. It never creates,
// initializes, or migrates the database and needs no owner lease; a missing or
// uninitialized database returns an fs.ErrNotExist error.
func Open(ctx context.Context, root, rootID string) (*Store, error) {
	path, err := existingDatabase(root, rootID)
	if err != nil {
		return nil, err
	}
	connector, err := sqlite.NewConnector(databaseURI(path, runtime.GOOS))
	if err != nil {
		return nil, fmt.Errorf("scheduler database connector: %w", err)
	}
	return openWithConnector(ctx, connector, path, rootID, runtime.GOOS)
}

// openWithConnector opens a Store over connector and verifies it in one
// guarded transaction before returning it.
func openWithConnector(ctx context.Context, connector driver.Connector, path, rootID, goos string) (*Store, error) {
	store := newStore(connector, path, rootID, goos)
	if err := store.transact(ctx, store.currentGuard, noStep); err != nil {
		return nil, errors.Join(err, store.db.Close())
	}
	return store, nil
}

// newStore owns one database handle with exactly one physical connection,
// open or idle; each operation reserves it for one transaction.
func newStore(connector driver.Connector, path, rootID, goos string) *Store {
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return &Store{db: db, path: path, rootID: rootID, goos: goos}
}

// Initialize creates the database of root as version 1 when it is absent or
// still an empty file, and accepts an existing current database of the same
// root. Only the scheduler owner-lease holder may call it; the lease creates
// the private scheduler directory.
func Initialize(ctx context.Context, root, rootID string) error {
	if err := task.ValidateRootID(rootID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path := DatabasePath(root)
	if err := checkPrivate(Directory(root), true); err != nil {
		return err
	}
	if err := createEmptyDatabase(path); err != nil {
		return err
	}
	if err := checkPrivate(path, false); err != nil {
		return err
	}
	connector, err := sqlite.NewConnector(databaseURI(path, runtime.GOOS))
	if err != nil {
		return fmt.Errorf("scheduler database connector: %w", err)
	}
	return initializeWithConnector(ctx, connector, path, rootID, runtime.GOOS, schemaObjects())
}

func initializeWithConnector(ctx context.Context, connector driver.Connector, path, rootID, goos string, objects []schemaObject) error {
	store := newStore(connector, path, rootID, goos)
	err := store.transact(ctx, store.connectionGuard, func(ctx context.Context, tx *sql.Tx) error {
		return store.initialize(ctx, tx, objects)
	})
	return errors.Join(err, store.db.Close())
}

func (s *Store) initialize(ctx context.Context, tx *sql.Tx, objects []schemaObject) error {
	version, err := userVersion(ctx, tx)
	if err != nil {
		return err
	}
	switch {
	case version == schemaVersion:
		return s.verifySchemaAndRoot(ctx, tx)
	case version == 1:
		return s.migrateV1(ctx, tx)
	case version > schemaVersion:
		return fmt.Errorf("%w: user_version %d", errNewerSchema, version)
	case version < 0:
		return fmt.Errorf("%w: user_version %d", errForeignDatabase, version)
	}
	eligible, err := s.uninitialized(ctx, tx)
	if err != nil {
		return err
	}
	if !eligible {
		return errForeignDatabase
	}
	for _, object := range objects {
		if _, err := tx.ExecContext(ctx, object.sql); err != nil {
			return fmt.Errorf("creating scheduler %s %s: %w", object.kind, object.name, classify(err))
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO scheduler_metadata (singleton, root_id) VALUES (1, ?)", s.rootID); err != nil {
		return classify(err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return classify(err)
	}
	return s.verifySchemaAndRoot(ctx, tx)
}

func noStep(context.Context, *sql.Tx) error { return nil }

// Close closes the database and its connections.
func (s *Store) Close() error { return s.db.Close() }

func existingDatabase(root, rootID string) (string, error) {
	if err := task.ValidateRootID(rootID); err != nil {
		return "", err
	}
	if err := checkPrivate(Directory(root), true); err != nil {
		return "", err
	}
	path := DatabasePath(root)
	return path, checkPrivate(path, false)
}

// createEmptyDatabase creates the private zero-byte database file exactly once
// and makes the file and its directory entry durable before SQLite opens it.
// An existing file is left unchanged for the guarded transaction to judge.
func createEmptyDatabase(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating scheduler database: %w", err)
	}
	if err := closeAfterBarrier(taskdir.BarrierFile(file), file.Close()); err != nil {
		return err
	}
	return taskdir.SyncDirectoryEntry(filepath.Dir(path))
}

// closeAfterBarrier reports both a barrier failure and a close failure of the
// new database file; neither hides the other.
func closeAfterBarrier(barrierErr, closeErr error) error {
	var faults []error
	if barrierErr != nil {
		faults = append(faults, fmt.Errorf("syncing new scheduler database: %w", barrierErr))
	}
	if closeErr != nil {
		faults = append(faults, fmt.Errorf("closing new scheduler database: %w", closeErr))
	}
	return errors.Join(faults...)
}

// checkPrivate requires a path the current user owns, of the expected type,
// with exactly mode 0700 for the directory or 0600 for the database, and no
// symbolic link at its final component.
func checkPrivate(path string, directory bool) error {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("%w: %s", errNotInitialized, path)
		}
		return fmt.Errorf("inspecting %s: %w", path, err)
	}
	if taskdir.CheckPrivateStat(&stat, os.Geteuid(), directory) != taskdir.PrivateStatAccepted {
		return fmt.Errorf("%w: %s", errUnsafeFile, path)
	}
	return nil
}
