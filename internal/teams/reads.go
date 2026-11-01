package teams

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/teams/contract"
)

func (c *Catalog) Lookup(ctx context.Context, ref task.TeamReference) (contract.Record, string, error) {
	if err := task.ValidateTeamReference(ref); err != nil {
		return contract.Record{}, "", err
	}
	var record contract.Record
	var catalogID string
	err := c.transact(ctx, catalogRead, func(ctx context.Context, tx *sql.Tx, id string) error {
		var err error
		record, err = lookup(ctx, tx, ref)
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

func lookup(ctx context.Context, tx *sql.Tx, ref task.TeamReference) (contract.Record, error) {
	key := keyFor(ref)
	var revision int64
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT revision, definition FROM teams
WHERE scope = ? AND project_kind = ? AND project_path = ? AND team_id = ?`,
		key.scope, key.kind, key.path, key.id).Scan(&revision, &data)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && data == nil) {
		return contract.Record{}, ErrNotFound
	}
	if err != nil {
		return contract.Record{}, unavailable(ctx, "reading Team", err)
	}
	return decodeRecord(ref, revision, data)
}

func decodeRecord(ref task.TeamReference, revision int64, data []byte) (contract.Record, error) {
	if err := task.ValidateTeamReference(ref); err != nil {
		return contract.Record{}, fmt.Errorf("%w: stored reference: %w", contract.ErrInvalidDefinition, err)
	}
	definition, err := contract.Decode(data)
	if err != nil {
		return contract.Record{}, err
	}
	if definition.ID != ref.TeamID || revision < 1 {
		return contract.Record{}, fmt.Errorf("%w: stored identity/revision mismatch", contract.ErrInvalidDefinition)
	}
	canonical, err := contract.Encode(definition)
	if err != nil {
		return contract.Record{}, err
	}
	if !bytes.Equal(canonical, data) {
		return contract.Record{}, fmt.Errorf("%w: stored definition is not canonical", contract.ErrInvalidDefinition)
	}
	return contract.Record{Reference: ref.Clone(), Revision: revision, Definition: definition}, nil
}

// ResolveMember reads the catalog identity and complete Team in one snapshot.
func (c *Catalog) ResolveMember(ctx context.Context, target task.TeamTarget) (contract.ResolvedMember, error) {
	if err := task.ValidateTeamTarget(target, false); err != nil {
		return contract.ResolvedMember{}, err
	}
	var result contract.ResolvedMember
	err := c.transact(ctx, catalogRead, func(ctx context.Context, tx *sql.Tx, id string) error {
		if target.CatalogID != "" && target.CatalogID != id {
			return ErrCatalogIdentity
		}
		record, err := lookup(ctx, tx, target.Team)
		if err != nil {
			return err
		}
		for _, member := range record.Definition.Members {
			if member.ID == target.MemberID {
				result = contract.ResolvedMember{CatalogID: id, Record: record, Member: member}
				return nil
			}
		}
		return ErrNotFound
	})
	if errors.Is(err, errMissingCatalog) {
		err = ErrNotFound
	}
	if err != nil {
		return contract.ResolvedMember{}, err
	}
	return result, nil
}

func (c *Catalog) List(ctx context.Context, project *task.TeamProject) (Listing, error) {
	result := Listing{Records: []contract.Record{}, Diagnostics: []Diagnostic{}}
	if project != nil {
		if err := validateProject(*project); err != nil {
			return result, err
		}
	}
	err := c.transact(ctx, catalogRead, func(ctx context.Context, tx *sql.Tx, id string) error {
		result.CatalogID = id
		return listRecords(ctx, tx, project, &result)
	})
	if errors.Is(err, errMissingCatalog) {
		err = nil
	}
	if err != nil {
		return Listing{}, err
	}
	return result, nil
}

func listRecords(ctx context.Context, tx *sql.Tx, project *task.TeamProject, result *Listing) (resultErr error) {
	kind, path := "", ""
	if project != nil {
		kind, path = project.Kind, project.Path
	}
	rows, err := tx.QueryContext(ctx, `SELECT scope, project_kind, project_path, team_id, revision, definition FROM teams
WHERE definition IS NOT NULL AND (scope = 'user' OR (scope = 'project' AND project_kind = ? AND project_path = ?))
ORDER BY scope, project_kind, project_path, team_id`, kind, path)
	if err != nil {
		return unavailable(ctx, "listing Teams", err)
	}
	defer func() { resultErr = joinClose(resultErr, rows.Close()) }()
	for rows.Next() {
		var key teamKey
		var revision int64
		var data []byte
		if err := rows.Scan(&key.scope, &key.kind, &key.path, &key.id, &revision, &data); err != nil {
			return unavailable(ctx, "reading Team listing", err)
		}
		ref := key.reference()
		record, err := decodeRecord(ref, revision, data)
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{
				Code: "invalid_record", Message: "Stored Team is invalid and cannot be selected.", Reference: &ref,
			})
			continue
		}
		result.Records = append(result.Records, record)
	}
	return unavailable(ctx, "reading Team listing", rows.Err())
}
