package taskdir

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	inspection "github.com/hishamkaram/delegation-layer/internal/inspection/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type rootContextHistoryEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Only canonical local request families retain local connector tokens. Receipts
// derive from these requests; remote references name another destination.
func (s *Store) rootContextHistory(ctx context.Context, expectedToken string) (digest string, tasks, inspections int, err error) {
	entries := []rootContextHistoryEntry{}
	for _, family := range []string{"tasks", "inspections"} {
		ids, scanErr := s.scanPrivateDirectoryIDs(ctx, filepath.Join(s.Root, family), family)
		if scanErr != nil {
			return "", 0, 0, scanErr
		}
		for _, id := range ids {
			entry, readErr := s.rootContextHistoryEntry(ctx, family, id, expectedToken)
			if readErr != nil {
				return "", 0, 0, fmt.Errorf("root context inventory %s/%s: %w", family, id, readErr)
			}
			entries = append(entries, entry)
		}
		if family == "tasks" {
			tasks = len(ids)
		} else {
			inspections = len(ids)
		}
	}
	// Include sorted typed paths so moving identical bytes between namespaces
	// changes the snapshot independently of filesystem enumeration order.
	slices.SortFunc(entries, func(left, right rootContextHistoryEntry) int { return strings.Compare(left.Path, right.Path) })
	digest, err = identityToken(entries)
	return digest, tasks, inspections, err
}

func (s *Store) rootContextHistoryEntry(ctx context.Context, family, id, expectedToken string) (rootContextHistoryEntry, error) {
	directory := filepath.Join(s.Root, family, id)
	if err := s.refuseRootContextStages(ctx, directory); err != nil {
		return rootContextHistoryEntry{}, err
	}
	var data []byte
	var connector *task.ConnectorContext
	name := "task.json"
	if family == "tasks" {
		td := &TaskDir{store: s, TaskID: id, Dir: directory}
		identity, err := td.recordedTaskIdentityContext(ctx)
		if err != nil {
			// A passive preparer has no admission fence or task owner leases.
			// Absence cannot safely classify a preparation/refusal fragment here.
			return rootContextHistoryEntry{}, fmt.Errorf("%w: cannot classify canonical task request: %w", task.ErrEvidenceFault, err)
		}
		data, connector = identity.data, identity.request.ConnectorContext
	} else {
		name = "request.json"
		var request inspection.RequestRecord
		var err error
		data, err = s.readBytesContext(ctx, filepath.Join(directory, name), task.MaxControlRecordSize)
		if err != nil {
			return rootContextHistoryEntry{}, err
		}
		if err = task.DecodeStrict(data, &request); err != nil {
			return rootContextHistoryEntry{}, err
		}
		if err = inspection.ValidateRequestRecord(request, s.RootID); err != nil {
			return rootContextHistoryEntry{}, err
		}
		if request.IdentityID() != id {
			return rootContextHistoryEntry{}, task.ErrIdentityMismatch
		}
		connector = request.Task.ConnectorContext
	}
	if connector != nil && connector.RootToken != expectedToken {
		return rootContextHistoryEntry{}, fmt.Errorf("%w: mixed connector root token history", task.ErrIdentityMismatch)
	}
	return rootContextHistoryEntry{Path: family + "/" + id + "/" + name, SHA256: task.ComputeSHA256(data)}, nil
}

func (s *Store) refuseRootContextStages(ctx context.Context, directory string) error {
	return s.refuseRootContextStagesExcept(ctx, directory, "")
}

func (s *Store) refuseRootContextStagesExcept(ctx context.Context, directory, ownedStage string) error {
	dir, err := s.openDir(directory)
	if err != nil {
		return err
	}
	return scanDirectoryNames(ctx.Err, dir, func(name string) error {
		if name != ownedStage && (strings.HasPrefix(name, "stage.") || strings.HasPrefix(name, "root-upgrade.") || strings.HasPrefix(name, "root-context.")) {
			return fmt.Errorf("%w: unresolved staging entry %s", task.ErrEvidenceFault, name)
		}
		return nil
	})
}
