package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
)

// Guards run as the first statements of every operation transaction. The
// immediate transaction already holds the RESERVED lock, which blocks a
// conversion to WAL and a concurrent migration until it ends. First failure
// wins: connection settings, journal mode, schema version, schema objects,
// then root identity. No guard repairs anything.

// connectionGuard verifies the connection-local settings and that the
// database uses the rollback journal this store requires.
func (s *Store) connectionGuard(ctx context.Context, tx *sql.Tx) error {
	expected := []struct {
		pragma string
		value  int64
	}{
		{pragma: "busy_timeout", value: busyTimeoutMillis},
		{pragma: "foreign_keys", value: 1},
		{pragma: "synchronous", value: synchronousExtra},
	}
	if s.goos == "darwin" {
		expected = append(expected, struct {
			pragma string
			value  int64
		}{pragma: "fullfsync", value: 1})
	}
	for _, setting := range expected {
		var value int64
		if err := tx.QueryRowContext(ctx, "PRAGMA "+setting.pragma).Scan(&value); err != nil {
			return classify(err)
		}
		if value != setting.value {
			return fmt.Errorf("%w: %s is %d", errConnectionConfig, setting.pragma, value)
		}
	}
	var mode string
	if err := tx.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return classify(err)
	}
	if !strings.EqualFold(mode, "delete") {
		return fmt.Errorf("%w: %q", errJournalMode, mode)
	}
	return nil
}

// currentGuard admits only an initialized current-version database of this
// root.
func (s *Store) currentGuard(ctx context.Context, tx *sql.Tx) error {
	if err := s.connectionGuard(ctx, tx); err != nil {
		return err
	}
	version, err := userVersion(ctx, tx)
	if err != nil {
		return err
	}
	switch {
	case version == schemaVersion:
		return s.verifySchemaAndRoot(ctx, tx)
	case version == 1:
		return contract.ErrMigrationRequired
	case version == 0:
		eligible, eligibleErr := s.uninitialized(ctx, tx)
		if eligibleErr != nil {
			return eligibleErr
		}
		if eligible {
			return errNotInitialized
		}
		return errForeignDatabase
	case version > schemaVersion:
		return fmt.Errorf("%w: user_version %d", errNewerSchema, version)
	default:
		return fmt.Errorf("%w: user_version %d", errForeignDatabase, version)
	}
}

func userVersion(ctx context.Context, tx *sql.Tx) (int64, error) {
	var version int64
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, classify(err)
	}
	return version, nil
}

// uninitialized reports whether the database is still the empty file the
// owner created. It runs inside the immediate transaction, after SQLite has
// rolled back any hot journal and while the RESERVED lock blocks other
// writers, so an interrupted initialization of the empty file is recognized
// again. SQLite reports one page even for an empty file, so the file length is
// the observation. A file with a header is never eligible, even with an empty
// schema.
func (s *Store) uninitialized(ctx context.Context, tx *sql.Tx) (bool, error) {
	var objects int64
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&objects); err != nil {
		return false, classify(err)
	}
	info, err := os.Stat(s.path)
	if err != nil {
		return false, fmt.Errorf("inspecting scheduler database: %w", err)
	}
	return objects == 0 && info.Size() == 0, nil
}

// verifySchemaAndRoot requires exactly the version 1 schema objects and the
// one metadata row naming this store's root.
func (s *Store) verifySchemaAndRoot(ctx context.Context, tx *sql.Tx) error {
	if err := verifySchemaObjects(ctx, tx); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT root_id FROM scheduler_metadata")
	if err != nil {
		return classify(err)
	}
	var roots []string
	err = scanEach(rows, func(rows *sql.Rows) error {
		var root string
		if scanErr := rows.Scan(&root); scanErr != nil {
			return scanErr
		}
		roots = append(roots, root)
		return nil
	})
	if err != nil {
		return err
	}
	if len(roots) != 1 {
		return fmt.Errorf("%w: %d metadata rows", errSchemaMismatch, len(roots))
	}
	if roots[0] != s.rootID {
		return errRootMismatch
	}
	return nil
}

func verifySchemaObjects(ctx context.Context, tx *sql.Tx) error {
	return verifyObjectSet(ctx, tx, schemaObjects())
}

func verifyObjectSet(ctx context.Context, tx *sql.Tx, objects []schemaObject) error {
	expected := make(map[string]schemaObject)
	for _, object := range objects {
		expected[object.name] = object
	}
	rows, err := tx.QueryContext(ctx, "SELECT type, name, sql FROM sqlite_schema")
	if err != nil {
		return classify(err)
	}
	seen := 0
	err = scanEach(rows, func(rows *sql.Rows) error {
		var kind, name string
		var text sql.NullString
		if scanErr := rows.Scan(&kind, &name, &text); scanErr != nil {
			return scanErr
		}
		if !text.Valid && kind == "index" && strings.HasPrefix(name, "sqlite_autoindex_") {
			return nil
		}
		object, known := expected[name]
		if !known || !text.Valid || object.kind != kind || object.sql != text.String {
			return fmt.Errorf("%w: unexpected %s %q", errSchemaMismatch, kind, name)
		}
		seen++
		return nil
	})
	if err != nil {
		return err
	}
	if seen != len(expected) {
		return fmt.Errorf("%w: %d of %d objects present", errSchemaMismatch, seen, len(expected))
	}
	return nil
}

// scanEach visits every row, then reports any iteration or close error. It
// always closes rows before returning.
func scanEach(rows *sql.Rows, visit func(*sql.Rows) error) error {
	var visitErr error
	for visitErr == nil && rows.Next() {
		visitErr = visit(rows)
	}
	return classify(errors.Join(visitErr, rows.Err(), rows.Close()))
}
