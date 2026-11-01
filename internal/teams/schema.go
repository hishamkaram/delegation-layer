package teams

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
)

type schemaObject struct {
	kind, name, sql string
}

func schemaObjects() []schemaObject {
	return []schemaObject{
		{"table", "catalog_metadata", `CREATE TABLE catalog_metadata (
	singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
	catalog_id TEXT NOT NULL CHECK (length(catalog_id) = 32 AND catalog_id NOT GLOB '*[^0-9a-f]*')
) STRICT`},
		{"trigger", "catalog_metadata_immutable", `CREATE TRIGGER catalog_metadata_immutable BEFORE UPDATE ON catalog_metadata
BEGIN SELECT RAISE(ABORT, 'catalog identity is immutable'); END`},
		{"trigger", "catalog_metadata_permanent", `CREATE TRIGGER catalog_metadata_permanent BEFORE DELETE ON catalog_metadata
BEGIN SELECT RAISE(ABORT, 'catalog identity is permanent'); END`},
		{"table", "teams", fmt.Sprintf(`CREATE TABLE teams (
	scope TEXT NOT NULL CHECK (scope IN ('user', 'project')),
	project_kind TEXT NOT NULL,
	project_path TEXT NOT NULL,
	team_id TEXT NOT NULL,
	revision INTEGER NOT NULL CHECK (revision > 0),
	definition BLOB CHECK (definition IS NULL OR length(definition) BETWEEN 1 AND %d),
	PRIMARY KEY (scope, project_kind, project_path, team_id),
	CHECK ((scope = 'user' AND project_kind = '' AND project_path = '') OR
		(scope = 'project' AND project_kind IN ('git', 'directory') AND project_path <> ''))
) STRICT`, task.MaxControlRecordSize)},
		{"trigger", "teams_permanent", `CREATE TRIGGER teams_permanent BEFORE DELETE ON teams
BEGIN SELECT RAISE(ABORT, 'team revision tombstones are permanent'); END`},
		{"table", "project_defaults", `CREATE TABLE project_defaults (
	project_kind TEXT NOT NULL CHECK (project_kind IN ('git', 'directory')),
	project_path TEXT NOT NULL CHECK (project_path <> ''),
	team_scope TEXT NOT NULL CHECK (team_scope IN ('user', 'project')),
	team_id TEXT NOT NULL,
	PRIMARY KEY (project_kind, project_path)
) STRICT`},
	}
}

// guardSchema initializes only an empty, version-zero database. Every other
// transaction verifies the entire known schema before reading or changing rows.
func (c *Catalog) guardSchema(ctx context.Context, tx *sql.Tx, initialize bool) (string, error) {
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return "", unavailable(ctx, "reading schema version (writer recovery may be required)", err)
	}
	// Reading user_version has pinned this transaction's snapshot. Check the
	// journal under that lock, rather than racing a mode change before BEGIN.
	if err := verifyJournal(ctx, tx); err != nil {
		return "", err
	}
	var applicationID int
	if err := tx.QueryRowContext(ctx, "PRAGMA application_id").Scan(&applicationID); err != nil {
		return "", unavailable(ctx, "reading application identity", err)
	}
	if applicationID != 0 {
		return "", fmt.Errorf("%w: foreign application identity", ErrSchema)
	}
	objects, err := readSchema(ctx, tx)
	if err != nil {
		return "", err
	}
	if initialize && version == 0 && len(objects) == 0 {
		info, statErr := os.Stat(c.path)
		if statErr != nil {
			return "", unavailable(ctx, "inspecting uninitialized catalog", statErr)
		}
		if info.Size() != 0 {
			return "", fmt.Errorf("%w: existing database is not an empty initialization file", ErrSchema)
		}
		if err := initializeSchema(ctx, tx); err != nil {
			return "", err
		}
	} else if version != 1 || !sameSchema(objects) {
		return "", fmt.Errorf("%w: unexpected schema or user_version %d", ErrSchema, version)
	}
	return readCatalogID(ctx, tx)
}

func readCatalogID(ctx context.Context, tx *sql.Tx) (string, error) {
	var id string
	if err := tx.QueryRowContext(ctx, "SELECT catalog_id FROM catalog_metadata WHERE singleton = 1").Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: missing catalog identity", ErrSchema)
		}
		return "", unavailable(ctx, "reading catalog identity", err)
	}
	if err := task.ValidateRootID(id); err != nil {
		return "", fmt.Errorf("%w: metadata identity: %w", ErrSchema, err)
	}
	return id, nil
}

func readSchema(ctx context.Context, tx *sql.Tx) (objects []schemaObject, resultErr error) {
	rows, err := tx.QueryContext(ctx, "SELECT type, name, sql FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'")
	if err != nil {
		return nil, unavailable(ctx, "reading schema", err)
	}
	defer func() { resultErr = joinClose(resultErr, rows.Close()) }()
	for rows.Next() {
		var object schemaObject
		if err := rows.Scan(&object.kind, &object.name, &object.sql); err != nil {
			return nil, unavailable(ctx, "decoding schema", err)
		}
		objects = append(objects, object)
	}
	return objects, unavailable(ctx, "reading schema", rows.Err())
}

func sameSchema(objects []schemaObject) bool {
	expected := schemaObjects()
	if len(objects) != len(expected) {
		return false
	}
	for _, object := range objects {
		found := false
		for _, candidate := range expected {
			if object == candidate {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func initializeSchema(ctx context.Context, tx *sql.Tx) error {
	id, err := randomid.NewRandomID()
	if err != nil {
		return unavailable(ctx, "generating catalog identity", err)
	}
	for _, object := range schemaObjects() {
		if _, createErr := tx.ExecContext(ctx, object.sql); createErr != nil {
			return unavailable(ctx, "initializing schema", createErr)
		}
	}
	if _, insertErr := tx.ExecContext(ctx, "INSERT INTO catalog_metadata (singleton, catalog_id) VALUES (1, ?)", id); insertErr != nil {
		return unavailable(ctx, "initializing identity", insertErr)
	}
	_, err = tx.ExecContext(ctx, "PRAGMA user_version = 1")
	return unavailable(ctx, "initializing version", err)
}
