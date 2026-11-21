package taskdir

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Only the sorted union of names survives collection. The flags preserve the
// requirement that an observed refusal must still exist when its ID is read.
type originMembershipID struct {
	id      string
	refusal bool
}

func (s *Store) originMembershipIDsContext(ctx context.Context) ([]originMembershipID, error) {
	tasks, err := s.scanPrivateDirectoryIDs(ctx, filepath.Join(s.Root, "tasks"), "task")
	if err != nil {
		return nil, err
	}
	ids := make([]originMembershipID, 0, len(tasks))
	for _, id := range tasks {
		ids = append(ids, originMembershipID{id: id})
	}
	refusals, err := s.originRefusalIDsContext(ctx)
	if err != nil {
		return nil, err
	}
	ids = append(ids, refusals...)
	slices.SortFunc(ids, func(a, b originMembershipID) int { return strings.Compare(a.id, b.id) })
	union := ids[:0]
	for _, id := range ids {
		if len(union) > 0 && union[len(union)-1].id == id.id {
			union[len(union)-1].refusal = union[len(union)-1].refusal || id.refusal
		} else {
			union = append(union, id)
		}
	}
	clear(ids[len(union):])
	return union, nil
}

func (s *Store) originRefusalIDsContext(ctx context.Context) ([]originMembershipID, error) {
	directory, err := s.openDir(filepath.Join(s.Root, "task-refusals"))
	if directoryAbsent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ids := []originMembershipID{}
	err = scanDirectoryNames(ctx.Err, directory, func(name string) error {
		if isStageFile(name) {
			return nil
		}
		id := strings.TrimSuffix(name, ".json")
		if name != id+".json" {
			return task.ErrEvidenceFault
		}
		if validationErr := task.ValidateTaskID(id); validationErr != nil {
			return validationErr
		}
		ids = append(ids, originMembershipID{id: id, refusal: true})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

func (s *Store) originMembershipEntryContext(ctx context.Context, gateway string, id originMembershipID) (*OriginInventoryEntry, error) {
	if !id.refusal {
		return s.originTaskEntryContext(ctx, gateway, id.id)
	}
	refusal, err := s.ReadNamespaceRefusalContext(ctx, id.id)
	if err != nil {
		return nil, err
	}
	if refusal == nil {
		return nil, task.ErrEvidenceFault
	}
	return s.originRefusalEntryContext(ctx, gateway, id.id, refusal)
}

// originMembershipDigest reproduces MarshalCanonical([]OriginInventoryEntry)
// exactly, including json.Marshal's HTML escaping and the final newline.
type originMembershipDigest struct {
	hash    hash.Hash
	started bool
}

func (d *originMembershipDigest) append(entry OriginInventoryEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	prefix := "["
	if d.started {
		prefix = ","
	}
	if _, err = io.WriteString(d.hash, prefix); err != nil {
		return err
	}
	d.started = true
	_, err = d.hash.Write(data)
	return err
}

func (d *originMembershipDigest) finish() (string, error) {
	tail := "]\n"
	if !d.started {
		tail = "[]\n"
	}
	if _, err := io.WriteString(d.hash, tail); err != nil {
		return "", err
	}
	return hex.EncodeToString(d.hash.Sum(nil)), nil
}

// Membership is immutable request/refusal identity only. Each decoded entry
// and its canonical JSON buffer are released before the next ID is read;
// only the visitor's bounded output page may retain selected entries.
func (s *Store) scanOriginMembershipContext(ctx context.Context, gateway string, visit func(OriginInventoryEntry) error) (string, error) {
	ids, err := s.originMembershipIDsContext(ctx)
	if err != nil {
		return "", err
	}
	digest := originMembershipDigest{hash: sha256.New()}
	for _, id := range ids {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		entry, readErr := s.originMembershipEntryContext(ctx, gateway, id)
		if readErr != nil {
			return "", readErr
		}
		if entry == nil {
			continue
		}
		if err = digest.append(*entry); err != nil {
			return "", err
		}
		if visit != nil {
			if err = visit(*entry); err != nil {
				return "", err
			}
		}
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	return digest.finish()
}

func (s *Store) originInventoryPageContext(ctx context.Context, state OriginAdmission, cursor string, limit int) (OriginInventory, error) {
	result := OriginInventory{Entries: []OriginInventoryEntry{}}
	membership, err := s.scanOriginMembershipContext(ctx, state.GatewayID, func(entry OriginInventoryEntry) error {
		if entry.TaskID <= cursor {
			return nil
		}
		if len(result.Entries) == limit {
			result.NextCursor = result.Entries[len(result.Entries)-1].TaskID
			return nil
		}
		result.Entries = append(result.Entries, entry)
		return nil
	})
	if err != nil {
		return OriginInventory{}, err
	}
	result.MembershipSHA256 = membership
	result.Complete = result.NextCursor == ""
	if result.Complete {
		result.CompletenessReceipt, err = originCompletenessToken(state, membership)
	}
	return result, err
}
