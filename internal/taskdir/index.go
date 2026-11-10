package taskdir

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// IndexKind selects an existing record family. It grants no control authority.
type IndexKind string

const (
	IndexLocal  IndexKind = "local"
	IndexRemote IndexKind = "remote"
)

func indexDirectory(kind IndexKind) (string, error) {
	switch kind {
	case IndexLocal:
		return "tasks", nil
	case IndexRemote:
		return "remote-refs", nil
	default:
		return "", fmt.Errorf("invalid index record family %q", kind)
	}
}

// IndexCandidate counts one directory entry, including invalid names and
// unsafe entries. Err is a source fault, never a fabricated task outcome.
type IndexCandidate struct {
	ID  string
	Err error
}

// IndexScan owns a directory descriptor. Next bounds enumeration work, not a
// portable resumable offset or an atomic snapshot. Close before its reader.
type IndexScan struct {
	mu     sync.Mutex
	reader *ReadOnlyStore
	dir    *os.File
	parent string
	done   bool
}

func (r *ReadOnlyStore) OpenIndexScan(ctx context.Context, kind IndexKind) (*IndexScan, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return nil, err
	}
	family, err := indexDirectory(kind)
	if err != nil {
		return nil, err
	}
	parent := filepath.Join(r.store.Root, family)
	dir, err := r.store.openDir(parent)
	if directoryAbsent(err) {
		return &IndexScan{reader: r, parent: parent, done: true}, nil
	}
	if err != nil {
		return nil, err
	}
	return &IndexScan{reader: r, dir: dir, parent: parent}, nil
}

func (s *IndexScan) Next(ctx context.Context, limit int) ([]IndexCandidate, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit < 1 || limit > 1024 {
		return nil, false, errors.New("index batch limit must be 1..1024")
	}
	s.reader.store.mu.Lock()
	defer s.reader.store.mu.Unlock()
	if err := s.reader.checkReadContext(ctx); err != nil {
		return nil, false, err
	}
	if s.done {
		return nil, true, nil
	}
	names, err := s.dir.Readdirnames(limit)
	if errors.Is(err, io.EOF) {
		s.done, err = true, nil
	}
	values := make([]IndexCandidate, 0, len(names))
	for _, name := range names {
		if cancelErr := ctx.Err(); cancelErr != nil {
			return values, false, cancelErr
		}
		values = append(values, IndexCandidate{ID: name, Err: s.reader.store.requirePrivateDirectoryCandidate(s.parent, name, "index")})
	}
	return values, s.done, err
}

func (s *IndexScan) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = true
	if s.dir == nil {
		return nil
	}
	err := s.dir.Close()
	s.dir = nil
	return err
}

// IndexMetadata is an allowlist for disposable search indexes. It deliberately
// excludes briefs, environment, executables, purpose text and provider output.
// Its digests identify records, not installed executables or launch authority.
type IndexMetadata struct {
	TaskID, Provider, Model, Effort, Cwd string
	WorkspaceID, TeamID, MemberID        string
	CreatedAt                            time.Time
	CreatedAtKnown                       bool
	RequestDigest, MetadataDigest        string
}

// ReadTaskIndexMetadata validates the immutable record fragments only. Missing
// meta.json is a partial admission with unknown creation time. This is not a
// prepared-set proof: it does not read the brief, logs, guards or lifecycle.
func (r *ReadOnlyStore) ReadTaskIndexMetadata(ctx context.Context, id string) (IndexMetadata, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return IndexMetadata{}, err
	}
	if err := task.ValidateTaskID(id); err != nil {
		return IndexMetadata{}, err
	}
	td := &TaskDir{store: r.store, TaskID: id, Dir: filepath.Join(r.store.Root, "tasks", id)}
	data, err := r.store.readBytesContext(ctx, filepath.Join(td.Dir, "task.json"), task.MaxControlRecordSize)
	if err != nil {
		return IndexMetadata{}, err
	}
	request, err := td.decodeTaskRequest(data)
	if err != nil {
		return IndexMetadata{}, err
	}
	value := IndexMetadata{
		TaskID: id, Provider: request.Provider, Model: request.RequestedConfig.Model, Effort: request.RequestedConfig.Effort,
		Cwd: request.CanonicalCwd, RequestDigest: task.ComputeSHA256(data),
	}
	if request.Workspace != nil {
		value.WorkspaceID = request.Workspace.ID
	}
	if request.Team != nil {
		value.TeamID, value.MemberID = request.Team.Target.Team.TeamID, request.Team.Target.MemberID
	}
	metaBytes, err := r.store.readBytesContext(ctx, filepath.Join(td.Dir, "meta.json"), task.MaxControlRecordSize)
	if directoryAbsent(err) {
		return value, nil
	}
	if err != nil {
		return IndexMetadata{}, err
	}
	var meta task.MetaRecord
	if err = task.DecodeStrict(metaBytes, &meta); err != nil {
		return IndexMetadata{}, err
	}
	if err = task.ValidateMetaRecord(&meta); err != nil {
		return IndexMetadata{}, err
	}
	// Reuse the prepared-set owner's matching rules without asserting that the
	// brief itself was read: this projection describes only the two records.
	if err = td.matchPrepared(&request, &meta, briefIdentity{sha256: request.BriefSHA256, length: request.BriefLength}, value.RequestDigest); err != nil {
		return IndexMetadata{}, err
	}
	value.CreatedAt, err = time.Parse(time.RFC3339Nano, meta.CreatedAt)
	if err != nil {
		return IndexMetadata{}, err
	}
	value.MetadataDigest = task.ComputeSHA256(metaBytes)
	value.CreatedAtKnown = true
	return value, ctx.Err()
}

// IndexEntryExists checks presence only, never following the candidate leaf.
func (r *ReadOnlyStore) IndexEntryExists(ctx context.Context, kind IndexKind, id string) (bool, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return false, err
	}
	if err := task.ValidateTaskID(id); err != nil {
		return false, err
	}
	family, err := indexDirectory(kind)
	if err != nil {
		return false, err
	}
	exists, err := r.store.entryExists(filepath.Join(r.store.Root, family, id))
	if directoryAbsent(err) {
		return false, nil
	}
	return exists, err
}
