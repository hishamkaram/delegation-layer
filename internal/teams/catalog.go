// Package teams owns the transactional Team catalog and project preferences.
// Catalog values contain configuration only; SQLite owns all mutable state.
package teams

import (
	"context"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/teams/contract"
)

var (
	ErrNotFound        = errors.New("team or member not found")
	ErrConflict        = errors.New("team revision conflict")
	ErrCatalogIdentity = errors.New("team catalog identity mismatch")
	ErrProjectContext  = errors.New("invalid team project context")
	ErrProjectMismatch = errors.New("team project mismatch")
	ErrUnavailable     = errors.New("team catalog unavailable")
	ErrSchema          = errors.New("unsupported or invalid team catalog schema")
)

type Catalog struct {
	path             string
	validateDefaults func(contract.ExecutionDefaults) error
}

// New captures the catalog location and optional known-support validator without
// I/O. The callback validates configuration; it must not execute providers.
func New(path string, validateDefaults func(contract.ExecutionDefaults) error) *Catalog {
	return &Catalog{path: path, validateDefaults: validateDefaults}
}

type Diagnostic struct {
	Code      string              `json:"code"`
	Message   string              `json:"message"`
	Reference *task.TeamReference `json:"reference,omitempty"`
}

type Listing struct {
	CatalogID   string            `json:"catalog_id,omitempty"`
	Records     []contract.Record `json:"records"`
	Diagnostics []Diagnostic      `json:"diagnostics"`
}

type DefaultResult struct {
	Status    string              `json:"status"`
	Reference *task.TeamReference `json:"reference,omitempty"`
	Record    *contract.Record    `json:"record,omitempty"`
}

func validateProject(project task.TeamProject) error {
	ref := task.TeamReference{Scope: "project", TeamID: "validation", Project: &project}
	if err := task.ValidateTeamReference(ref); err != nil {
		return fmt.Errorf("%w: %w", ErrProjectContext, err)
	}
	return nil
}

type teamKey struct {
	scope, kind, path, id string
}

func keyFor(ref task.TeamReference) teamKey {
	key := teamKey{scope: ref.Scope, id: ref.TeamID}
	if ref.Project != nil {
		key.kind, key.path = ref.Project.Kind, ref.Project.Path
	}
	return key
}

func (key teamKey) reference() task.TeamReference {
	ref := task.TeamReference{Scope: key.scope, TeamID: key.id}
	if key.scope == "project" {
		ref.Project = &task.TeamProject{Kind: key.kind, Path: key.path}
	}
	return ref
}

func (c *Catalog) definition(ref task.TeamReference, definition contract.Team) ([]byte, error) {
	if err := task.ValidateTeamReference(ref); err != nil {
		return nil, err
	}
	if ref.TeamID != definition.ID {
		return nil, fmt.Errorf("%w: definition ID must match the reference", contract.ErrInvalidDefinition)
	}
	data, err := contract.Encode(definition)
	if err != nil {
		return nil, err
	}
	if c.validateDefaults != nil {
		for _, member := range definition.Members {
			if member.ExecutionDefaults != nil {
				if err := c.validateDefaults(*member.ExecutionDefaults); err != nil {
					return nil, fmt.Errorf("%w: member %s defaults: %w", contract.ErrInvalidDefinition, member.ID, err)
				}
			}
		}
	}
	return data, nil
}

func unavailable(ctx context.Context, action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, action, errors.Join(err, ctx.Err()))
}
