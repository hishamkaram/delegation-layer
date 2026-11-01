package teams

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/teams/contract"
)

func (c *Catalog) Register(ctx context.Context, ref task.TeamReference, definition contract.Team) (contract.Record, string, error) {
	return c.save(ctx, ref, 0, definition)
}

func (c *Catalog) Update(ctx context.Context, ref task.TeamReference, expected int64, definition contract.Team) (contract.Record, string, error) {
	if expected < 1 {
		return contract.Record{}, "", fmt.Errorf("%w: expected revision must be positive", ErrConflict)
	}
	return c.save(ctx, ref, expected, definition)
}

func (c *Catalog) save(ctx context.Context, ref task.TeamReference, expected int64, definition contract.Team) (contract.Record, string, error) {
	data, err := c.definition(ref, definition)
	if err != nil {
		return contract.Record{}, "", err
	}
	var record contract.Record
	var catalogID string
	mode := catalogWrite
	if expected == 0 {
		mode = catalogCreate
	}
	err = c.transact(ctx, mode, func(ctx context.Context, tx *sql.Tx, id string) error {
		revision, revisionErr := nextRevision(ctx, tx, ref, expected)
		if revisionErr != nil {
			return revisionErr
		}
		key := keyFor(ref)
		_, err = tx.ExecContext(ctx, `INSERT INTO teams (scope, project_kind, project_path, team_id, revision, definition)
VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (scope, project_kind, project_path, team_id)
DO UPDATE SET revision = excluded.revision, definition = excluded.definition`,
			key.scope, key.kind, key.path, key.id, revision, data)
		if err != nil {
			return unavailable(ctx, "saving Team", err)
		}
		record, err = decodeRecord(ref, revision, data)
		catalogID = id
		return err
	})
	if errors.Is(err, errMissingCatalog) {
		err = ErrNotFound
	}
	if err != nil {
		return contract.Record{}, "", err
	}
	return record, catalogID, nil
}

// nextRevision runs under BEGIN IMMEDIATE. A tombstone is unavailable to update
// but remains the revision authority when registering that identity again.
func nextRevision(ctx context.Context, tx *sql.Tx, ref task.TeamReference, expected int64) (int64, error) {
	key := keyFor(ref)
	var revision int64
	var active bool
	err := tx.QueryRowContext(ctx, `SELECT revision, definition IS NOT NULL FROM teams
WHERE scope = ? AND project_kind = ? AND project_path = ? AND team_id = ?`,
		key.scope, key.kind, key.path, key.id).Scan(&revision, &active)
	if errors.Is(err, sql.ErrNoRows) {
		if expected != 0 {
			return 0, ErrNotFound
		}
		return 1, nil
	}
	if err != nil {
		return 0, unavailable(ctx, "reading Team revision", err)
	}
	if expected != 0 && !active {
		return 0, ErrNotFound
	}
	if (expected == 0 && active) || (expected != 0 && expected != revision) || revision == math.MaxInt64 {
		return 0, ErrConflict
	}
	if revision < 1 {
		return 0, fmt.Errorf("%w: invalid stored revision", ErrSchema)
	}
	return revision + 1, nil
}

func (c *Catalog) Delete(ctx context.Context, ref task.TeamReference, expected int64) (int64, error) {
	if err := task.ValidateTeamReference(ref); err != nil {
		return 0, err
	}
	if expected < 1 {
		return 0, fmt.Errorf("%w: expected revision must be positive", ErrConflict)
	}
	var revision int64
	err := c.transact(ctx, catalogWrite, func(ctx context.Context, tx *sql.Tx, _ string) error {
		var err error
		revision, err = nextRevision(ctx, tx, ref, expected)
		if err != nil {
			return err
		}
		key := keyFor(ref)
		_, err = tx.ExecContext(ctx, `UPDATE teams SET revision = ?, definition = NULL
WHERE scope = ? AND project_kind = ? AND project_path = ? AND team_id = ?`,
			revision, key.scope, key.kind, key.path, key.id)
		return unavailable(ctx, "deleting Team", err)
	})
	if errors.Is(err, errMissingCatalog) {
		err = ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return revision, nil
}
