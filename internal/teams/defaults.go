package teams

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Default observes preferences; it never selects a replacement or clears a stale
// reference. Status is unset, resolved or stale.
func (c *Catalog) Default(ctx context.Context, project task.TeamProject) (DefaultResult, error) {
	if err := validateProject(project); err != nil {
		return DefaultResult{}, err
	}
	result := DefaultResult{Status: "unset"}
	err := c.transact(ctx, catalogRead, func(ctx context.Context, tx *sql.Tx, _ string) error {
		var ref task.TeamReference
		err := tx.QueryRowContext(ctx, `SELECT team_scope, team_id FROM project_defaults
WHERE project_kind = ? AND project_path = ?`, project.Kind, project.Path).Scan(&ref.Scope, &ref.TeamID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return unavailable(ctx, "reading project default", err)
		}
		if ref.Scope == "project" {
			ref.Project = &project
		}
		if validationErr := task.ValidateTeamReference(ref); validationErr != nil {
			return fmt.Errorf("%w: project default: %w", ErrSchema, validationErr)
		}
		result.Reference = &ref
		record, err := lookup(ctx, tx, ref)
		if errors.Is(err, ErrNotFound) {
			result.Status = "stale"
			return nil
		}
		if err != nil {
			return err
		}
		result.Status, result.Record = "resolved", &record
		return nil
	})
	if errors.Is(err, errMissingCatalog) {
		err = nil
	}
	if err != nil {
		return DefaultResult{}, err
	}
	return result, nil
}

func (c *Catalog) SetDefault(ctx context.Context, project task.TeamProject, ref task.TeamReference) error {
	if err := validateProject(project); err != nil {
		return err
	}
	if err := task.ValidateTeamReference(ref); err != nil {
		return err
	}
	if ref.Project != nil && *ref.Project != project {
		return ErrProjectMismatch
	}
	err := c.transact(ctx, catalogWrite, func(ctx context.Context, tx *sql.Tx, _ string) error {
		if _, err := lookup(ctx, tx, ref); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO project_defaults (project_kind, project_path, team_scope, team_id)
VALUES (?, ?, ?, ?) ON CONFLICT (project_kind, project_path)
DO UPDATE SET team_scope = excluded.team_scope, team_id = excluded.team_id`,
			project.Kind, project.Path, ref.Scope, ref.TeamID)
		return unavailable(ctx, "setting project default", err)
	})
	if errors.Is(err, errMissingCatalog) {
		return ErrNotFound
	}
	return err
}

func (c *Catalog) ClearDefault(ctx context.Context, project task.TeamProject) error {
	if err := validateProject(project); err != nil {
		return err
	}
	err := c.transact(ctx, catalogWrite, func(ctx context.Context, tx *sql.Tx, _ string) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM project_defaults WHERE project_kind = ? AND project_path = ?", project.Kind, project.Path)
		return unavailable(ctx, "clearing project default", err)
	})
	if errors.Is(err, errMissingCatalog) {
		return nil
	}
	return err
}
