package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
)

// migrateV1 preserves the referenced schedules parent and reconstructs only
// skip_spans, which has no incoming foreign keys. Canonical DDL is the guard's
// authority; ALTER-generated spelling is not a compatible schema.
func (s *Store) migrateV1(ctx context.Context, tx *sql.Tx) error {
	if err := verifyObjectSet(ctx, tx, legacySchemaObjects()); err != nil {
		return err
	}
	var root string
	if err := tx.QueryRowContext(ctx, "SELECT root_id FROM scheduler_metadata WHERE singleton = 1").Scan(&root); err != nil {
		return classify(err)
	}
	if root != s.rootID {
		return errRootMismatch
	}
	if err := rebuildSkipSpans(ctx, tx); err != nil {
		return err
	}

	for _, object := range reliabilityObjects() {
		if _, err := tx.ExecContext(ctx, object.sql); err != nil {
			return classify(err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return classify(err)
	}
	return s.verifySchemaAndRoot(ctx, tx)
}

func rebuildSkipSpans(ctx context.Context, tx *sql.Tx) error {
	for _, query := range []string{
		"CREATE TEMP TABLE saved_skip_spans AS SELECT * FROM skip_spans",
		"DROP TRIGGER skip_spans_immutable", "DROP TRIGGER skip_spans_permanent", "DROP TABLE skip_spans",
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return classify(err)
		}
	}
	for _, object := range schemaObjects() {
		if object.name == "skip_spans" || object.name == "skip_spans_immutable" || object.name == "skip_spans_permanent" {
			if _, err := tx.ExecContext(ctx, object.sql); err != nil {
				return classify(err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO skip_spans SELECT * FROM saved_skip_spans"); err != nil {
		return classify(err)
	}
	if _, err := tx.ExecContext(ctx, "DROP TABLE saved_skip_spans"); err != nil {
		return classify(err)
	}
	return nil
}
