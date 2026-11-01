package teams

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"modernc.org/sqlite"
)

var errMissingCatalog = errors.New("team catalog does not exist")

const operationTimeout = 5 * time.Second

type catalogMode uint8

const (
	catalogRead catalogMode = iota
	catalogWrite
	catalogCreate
)

// databaseURI never treats a path's ?, # or % as SQLite options. Reads restrict
// SQLite's open flags; query_only alone would still create a missing database.
func databaseURI(path string, write bool) string {
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := url.Values{"mode": {"ro"}, "_busy_timeout": {"5000"}}
	if write {
		query.Set("mode", "rw")
		query.Set("_txlock", "immediate")
		query.Set("_synchronous", "EXTRA")
		if runtime.GOOS == "darwin" {
			query.Add("_pragma", "fullfsync(1)")
		}
	}
	uri.RawQuery = query.Encode()
	return uri.String()
}

func (c *Catalog) transact(ctx context.Context, mode catalogMode, body func(context.Context, *sql.Tx, string) error) (resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return unavailable(ctx, "opening catalog", err)
	}
	if err := c.prepare(ctx, mode); err != nil {
		return err
	}
	write := mode != catalogRead
	connector, err := sqlite.NewConnector(databaseURI(c.path, write))
	if err != nil {
		return unavailable(ctx, "opening connector", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	defer func() { resultErr = joinClose(resultErr, db.Close()) }()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: !write})
	if err != nil {
		return unavailable(ctx, "beginning transaction (writer recovery may be required)", err)
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if !errors.Is(rollbackErr, sql.ErrTxDone) {
			resultErr = joinClose(resultErr, rollbackErr)
		}
	}()
	if write {
		err = verifyDurability(ctx, tx)
	}
	var id string
	if err == nil {
		id, err = c.guardSchema(ctx, tx, mode == catalogCreate)
	}
	if err == nil {
		err = body(ctx, tx, id)
	}
	if err != nil {
		return err
	}
	// database/sql's driver Commit has no context parameter. Bound its native
	// busy wait to the time left on this operation before handing it ownership.
	if write {
		if err := boundCommit(ctx, tx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return unavailable(ctx, "committing transaction; inspect durable state before retrying mutation", err)
	}
	return nil
}

func boundCommit(ctx context.Context, tx *sql.Tx) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return fmt.Errorf("%w: missing catalog operation deadline", ErrUnavailable)
	}
	remaining := max(0, time.Until(deadline).Milliseconds())
	_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", remaining))
	return unavailable(ctx, "bounding commit wait", err)
}

func joinClose(resultErr, closeErr error) error {
	if closeErr != nil {
		return errors.Join(resultErr, fmt.Errorf("%w: closing catalog resource: %w", ErrUnavailable, closeErr))
	}
	return resultErr
}

func verifyJournal(ctx context.Context, tx *sql.Tx) error {
	var mode string
	if err := tx.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return unavailable(ctx, "reading journal mode (writer recovery may be required)", err)
	}
	if mode != "delete" {
		return fmt.Errorf("%w: expected DELETE rollback journal, found %s", ErrSchema, mode)
	}
	return nil
}

func verifyDurability(ctx context.Context, tx *sql.Tx) error {
	settings := []struct {
		name string
		want int
	}{{"synchronous", 3}}
	if runtime.GOOS == "darwin" {
		settings = append(settings, struct {
			name string
			want int
		}{"fullfsync", 1})
	}
	for _, setting := range settings {
		var value int
		if err := tx.QueryRowContext(ctx, "PRAGMA "+setting.name).Scan(&value); err != nil {
			return unavailable(ctx, "checking durability", err)
		}
		if value != setting.want {
			return fmt.Errorf("%w: required %s=%d", ErrUnavailable, setting.name, setting.want)
		}
	}
	return nil
}

func (c *Catalog) prepare(ctx context.Context, mode catalogMode) error {
	if !filepath.IsAbs(c.path) || filepath.Clean(c.path) != c.path {
		return fmt.Errorf("%w: catalog path must be absolute and clean", ErrUnavailable)
	}
	if mode == catalogCreate {
		if err := createDatabase(ctx, c.path); err != nil {
			return err
		}
	}
	info, err := os.Lstat(c.path)
	if errors.Is(err, fs.ErrNotExist) {
		return errMissingCatalog
	}
	if err != nil {
		return fmt.Errorf("%w: inspecting catalog: %w", ErrUnavailable, err)
	}
	if !info.Mode().IsRegular() || (mode != catalogRead && info.Mode().Perm() != 0o600) {
		return fmt.Errorf("%w: catalog must be a regular file (0600 for mutation)", ErrUnavailable)
	}
	if mode != catalogRead {
		return privateDirectory(filepath.Dir(c.path))
	}
	return nil
}

func createDatabase(ctx context.Context, path string) error {
	directory := filepath.Dir(path)
	if err := ensureDirectory(ctx, directory); err != nil {
		return err
	}
	if err := privateDirectory(directory); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: creating catalog file: %w", ErrUnavailable, err)
	}
	barrierErr := taskdir.BarrierFile(file)
	closeErr := file.Close()
	if err := errors.Join(barrierErr, closeErr); err != nil {
		return fmt.Errorf("%w: syncing new catalog: %w", ErrUnavailable, err)
	}
	if err := taskdir.SyncDirectoryEntry(directory); err != nil {
		return fmt.Errorf("%w: syncing catalog directory entry: %w", ErrUnavailable, err)
	}
	return nil
}

func privateDirectory(directory string) error {
	info, err := os.Stat(directory)
	if err != nil {
		return fmt.Errorf("%w: inspecting catalog directory: %w", ErrUnavailable, err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%w: catalog directory must be private (0700)", ErrUnavailable)
	}
	return nil
}

// Persist each newly created ancestor before creating descendants. A racing
// initializer also barriers the existing parent before relying on its entry.
func ensureDirectory(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return unavailable(ctx, "creating catalog directory", err)
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if parentErr := ensureDirectory(ctx, filepath.Dir(path)); parentErr != nil {
			return parentErr
		}
		if mkdirErr := os.Mkdir(path, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, fs.ErrExist) {
			return unavailable(ctx, "creating catalog directory", mkdirErr)
		}
	} else if err != nil {
		return unavailable(ctx, "inspecting catalog directory", err)
	} else if !info.IsDir() {
		return fmt.Errorf("%w: catalog ancestor must be a directory", ErrUnavailable)
	}
	if err := taskdir.SyncDirectoryEntry(path); err != nil {
		return unavailable(ctx, "syncing catalog directory", err)
	}
	return nil
}
