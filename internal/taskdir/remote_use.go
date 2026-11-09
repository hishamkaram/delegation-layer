package taskdir

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/remotedata"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

var ErrRemoteDataRetired = errors.New("remote data has been retired")

// RemoteByteLease excludes retirement while bytes are in use. It confers no
// task admission or process-lifetime authority. Close before its borrowed Store.
type RemoteByteLease struct {
	store       *Store
	lock        *LockFile
	maintenance bool
	unlocked    bool
}

type RemoteByteCleanupError struct {
	lease *RemoteByteLease
	cause error
}

func (e *RemoteByteCleanupError) Error() string {
	return "remote byte lease cleanup incomplete: " + e.cause.Error()
}
func (e *RemoteByteCleanupError) Unwrap() error         { return e.cause }
func (e *RemoteByteCleanupError) RetainedStore() *Store { return e.lease.store }
func (e *RemoteByteCleanupError) RetryClose() error     { return e.lease.Close() }

func (s *Store) AcquireRemoteByteUse(ctx context.Context, id string, origin, exclusive bool) (lease *RemoteByteLease, resultErr error) {
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	journal, err := s.openRemoteDataJournal(ctx, id, origin, true)
	if err != nil {
		return nil, err
	}
	if err = journal.Close(); err != nil {
		return nil, err
	}
	lease = &RemoteByteLease{store: s}
	if err = s.maintLock.LockSH(ctx); err != nil {
		return nil, err
	}
	lease.maintenance = true
	family := "remote-inputs"
	if origin {
		family = "remote-refs"
	}
	lease.lock, err = s.openLock(filepath.Join(s.Root, family, id, ".byte-use.lock"), LockLevelNone)
	if err == nil {
		if exclusive {
			err = lease.lock.LockEXNonblockingContext(ctx)
		} else {
			err = lease.lock.LockSH(ctx)
		}
	}
	if err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	return lease, nil
}

func (l *RemoteByteLease) Close() error {
	if l == nil {
		return nil
	}
	if l.lock != nil {
		if !l.unlocked {
			if err := l.lock.Unlock(); err != nil {
				return &RemoteByteCleanupError{l, err}
			}
			l.unlocked = true
		}
		if err := l.lock.Close(); err != nil {
			return &RemoteByteCleanupError{l, err}
		}
		l.lock = nil
	}
	if l.maintenance {
		if err := l.store.maintLock.Unlock(); err != nil {
			return &RemoteByteCleanupError{l, err}
		}
		l.maintenance = false
	}
	return nil
}

func (s *Store) openRemoteDataJournal(ctx context.Context, id string, origin, create bool) (*ControlDir, error) {
	if origin {
		return s.OpenRemoteReferenceContext(ctx, id, create)
	}
	return s.OpenRemoteInputJournalContext(ctx, id, create)
}

// RemoteUseRecord is an immutable dependency, not a cached consumer state.
// Retirement must read the consumer's canonical task and closure authorities.
type RemoteUseRecord struct {
	Root   string `json:"root"`
	RootID string `json:"root_id"`
	TaskID string `json:"task_id"`
	Cwd    string `json:"cwd"`
}

func RemoteUseName(use RemoteUseRecord) (string, error) {
	if err := task.ValidateRootID(use.RootID); err != nil {
		return "", err
	}
	if err := task.ValidateTaskID(use.TaskID); err != nil {
		return "", err
	}
	if !filepath.IsAbs(use.Root) || !filepath.IsAbs(use.Cwd) || filepath.Clean(use.Root) != use.Root || filepath.Clean(use.Cwd) != use.Cwd {
		return "", task.ErrEvidenceFault
	}
	pair := struct {
		RootID string `json:"consumer_root_id"`
		TaskID string `json:"consumer_task_id"`
	}{use.RootID, use.TaskID}
	b, err := task.MarshalCanonical(pair)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "use-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])) + ".json", nil
}

type RemoteDataUse struct {
	owner   *Store
	journal *ControlDir
	lease   *RemoteByteLease
	record  RemoteUseRecord
}

// BeginRemoteDataUse protects preparation before any provider inspection, then
// Register publishes the durable dependency immediately before task creation.
func (s *Store) BeginRemoteDataUse(ctx context.Context, request task.TaskRecord) (use *RemoteDataUse, resultErr error) {
	loc, managed, err := remotedata.Classify(request.CanonicalCwd)
	if err != nil || !managed {
		return nil, err
	}
	if s.ownsRemoteInputWorkspace(loc, request) {
		return nil, nil
	}
	owner, err := OpenStoreWithPredicatesContext(ctx, loc.Root, s.predicates)
	if err != nil {
		return nil, err
	}
	use = &RemoteDataUse{owner: owner, record: RemoteUseRecord{Root: s.Root, RootID: s.RootID, TaskID: request.TaskID, Cwd: request.CanonicalCwd}}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, use.Close())
			use = nil
		}
	}()
	if owner.RootID != loc.RootID {
		return use, task.ErrIdentityMismatch
	}
	if err = validateRemoteUseOwner(ctx, owner, loc); err != nil {
		return use, err
	}
	use.lease, err = owner.AcquireRemoteByteUse(ctx, loc.TaskID, loc.Role == remotedata.Origin, false)
	if err != nil {
		return use, err
	}
	use.journal, err = owner.openRemoteDataJournal(ctx, loc.TaskID, loc.Role == remotedata.Origin, false)
	if err != nil {
		return use, err
	}
	err = use.journal.WithLock(ctx, func(tx *ControlTransaction) error { return remoteDataNotRetired(tx) })
	return use, err
}

func (s *Store) ownsRemoteInputWorkspace(loc remotedata.Location, request task.TaskRecord) bool {
	if loc.Root != s.Root || loc.RootID != s.RootID || loc.TaskID != request.TaskID || loc.Role != remotedata.Destination || loc.Family != remotedata.Inputs || request.Remote == nil || request.Remote.InputSHA256 == "" || request.Remote.StorageLayout != loc.Layout {
		return false
	}
	workspace, err := remotedata.InputWorkspace(loc.Layout, loc.Root, loc.RootID, loc.TaskID)
	return err == nil && request.CanonicalCwd == workspace
}

func remoteDataNotRetired(tx *ControlTransaction) error {
	names, err := tx.RecordNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		if name == "retirement.json" {
			return ErrRemoteDataRetired
		}
	}
	return nil
}

func (u *RemoteDataUse) Register(ctx context.Context) error {
	if u == nil {
		return nil
	}
	name, err := RemoteUseName(u.record)
	if err != nil {
		return err
	}
	return u.journal.WithLock(ctx, func(tx *ControlTransaction) error {
		if e := remoteDataNotRetired(tx); e != nil {
			return e
		}
		_, e := tx.Put(name, u.record)
		return e
	})
}

type RemoteUseCleanupError struct {
	use   *RemoteDataUse
	cause error
}

func (e *RemoteUseCleanupError) Error() string {
	return "remote data use cleanup incomplete: " + e.cause.Error()
}
func (e *RemoteUseCleanupError) Unwrap() error         { return e.cause }
func (e *RemoteUseCleanupError) RetainedStore() *Store { return e.use.owner }
func (e *RemoteUseCleanupError) RetryClose() error     { return e.use.Close() }
func (u *RemoteDataUse) Close() error {
	if u == nil {
		return nil
	}
	if u.journal != nil {
		if err := u.journal.Close(); err != nil {
			return &RemoteUseCleanupError{u, err}
		}
		u.journal = nil
	}
	if u.lease != nil {
		if err := u.lease.Close(); err != nil {
			return &RemoteUseCleanupError{u, err}
		}
		u.lease = nil
	}
	if u.owner != nil {
		if err := u.owner.Close(); err != nil {
			return &RemoteUseCleanupError{u, err}
		}
		u.owner = nil
	}
	return nil
}

// ReadRemoteRecord reads only a named, bounded journal record, without creating
// locks or directories. capture selects the destination capture receipt owner.
func (r *ReadOnlyStore) ReadRemoteRecord(ctx context.Context, id string, origin, capture bool, name string, value any) error {
	data, err := r.remoteRecordBytes(ctx, id, origin, capture, name)
	if err != nil {
		return err
	}
	return task.DecodeStrict(data, value)
}

// ReadCanonicalRemoteRecord is for immutable records whose owner requires
// canonical serialization as well as a valid decoded value.
func (r *ReadOnlyStore) ReadCanonicalRemoteRecord(ctx context.Context, id string, origin, capture bool, name string, value any) error {
	data, err := r.remoteRecordBytes(ctx, id, origin, capture, name)
	if err != nil {
		return err
	}
	if err = task.DecodeStrict(data, value); err != nil {
		return err
	}
	canonical, err := task.MarshalCanonical(value)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, canonical) {
		return task.ErrEvidenceFault
	}
	return nil
}

func (r *ReadOnlyStore) remoteRecordBytes(ctx context.Context, id string, origin, capture bool, name string) ([]byte, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return nil, err
	}
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	if err := validateControlName(name); err != nil {
		return nil, err
	}
	family := "remote-inputs"
	if origin {
		family = "remote-refs"
	}
	if capture {
		if origin {
			return nil, fmt.Errorf("origin has no capture journal")
		}
		family = "remote-captures"
	}
	data, err := r.store.readBytes(filepath.Join(r.store.Root, family, id, name), task.MaxControlRecordSize)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (r *ReadOnlyStore) RemoteRecordNames(ctx context.Context, id string, origin bool) (names []string, resultErr error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return nil, err
	}
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	family := "remote-inputs"
	if origin {
		family = "remote-refs"
	}
	f, err := r.store.openDir(filepath.Join(r.store.Root, family, id))
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	for {
		if err = ctx.Err(); err != nil {
			return names, err
		}
		entries, e := f.ReadDir(100)
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			if err = validateControlName(entry.Name()); err != nil {
				return names, err
			}
			names = append(names, entry.Name())
		}
		if errors.Is(e, io.EOF) {
			return names, nil
		}
		if e != nil {
			return names, e
		}
	}
}

// RemoteTaskClosed validates terminal and canonical closure facts without
// acquiring recovery authority or starting commands.
func (r *ReadOnlyStore) RemoteTaskClosed(ctx context.Context, id string) (bool, error) {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	if err := r.checkReadContext(ctx); err != nil {
		return false, err
	}
	exists, err := r.store.TaskEntryExists(id)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	td := &TaskDir{store: r.store, TaskID: id, Dir: filepath.Join(r.store.Root, "tasks", id)}
	inspection, err := td.Inspect()
	if err != nil || !inspection.Terminal() {
		return false, err
	}
	// Inspect retains canonical full winner validation. Closure needs no log
	// descriptors or second pass over the same captured bytes.
	evidence, err := LoadLifetimeEvidenceContext(ctx, td)
	if err != nil {
		return false, err
	}
	closure, err := task.ResolveLifetimeClosure(evidence)
	return closure.Closed(), errors.Join(err, ctx.Err())
}

// RemoteReader preserves this installation's predicate registry for exact-root
// consumer evidence; it does not initialize or probe that root.
func (s *Store) RemoteReader(ctx context.Context, root string) (*ReadOnlyStore, error) {
	return OpenReadOnlyStoreContext(ctx, root, s.predicates)
}

func (r *ReadOnlyStore) RemoteExecutionReleased(ctx context.Context, id string) (released bool, resultErr error) {
	if err := task.ValidateTaskID(id); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	lock, err := r.store.openExistingLock(filepath.Join(r.store.Root, "tasks", id, ".run.lock"), LockLevelRun)
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	if err = lock.LockEXNonblockingContext(ctx); errors.Is(err, task.ErrLockBusy) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, lock.Unlock()
}

func (r *ReadOnlyStore) RemoteReader(ctx context.Context, root string) (*ReadOnlyStore, error) {
	return OpenReadOnlyStoreContext(ctx, root, r.store.predicates)
}

func validateRemoteUseOwner(ctx context.Context, owner *Store, loc remotedata.Location) error {
	if loc.Role == remotedata.Origin {
		return validateRemoteSnapshotOwner(ctx, owner, loc)
	}
	return validateRemoteDestinationOwner(owner, loc)
}

func validateRemoteSnapshotOwner(ctx context.Context, owner *Store, loc remotedata.Location) (resultErr error) {
	journal, err := owner.OpenRemoteReferenceContext(ctx, loc.TaskID, false)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, journal.Close()) }()
	names, err := journal.RecordNamesContext(ctx)
	if err != nil {
		return err
	}
	ready, frozen := false, false
	for _, n := range names {
		ready = ready || n == "manifest-ready.json"
		frozen = frozen || n == "frozen.json"
	}
	if !ready || !frozen {
		return task.ErrEvidenceFault
	}
	var layout task.RemoteOriginLayout
	err = journal.Read("origin-layout.json", &layout)
	if task.RecordAbsent(err) {
		layout.StorageLayout = task.RemoteDataLegacy
	} else if err != nil {
		return err
	}
	if layout.StorageLayout != loc.Layout {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateRemoteDestinationOwner(owner *Store, loc remotedata.Location) (resultErr error) {
	td, err := owner.OpenTask(loc.TaskID)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	request, _, err := td.PreparedRecords()
	if err != nil {
		return err
	}
	if request.Remote == nil || request.Remote.StorageLayout != loc.Layout {
		return task.ErrIdentityMismatch
	}
	if loc.Family == remotedata.Inputs {
		cwd, e := remotedata.InputWorkspace(loc.Layout, loc.Root, loc.RootID, loc.TaskID)
		if e != nil {
			return e
		}
		if request.Remote.InputSHA256 == "" || request.CanonicalCwd != cwd {
			return task.ErrIdentityMismatch
		}
	} else if len(request.Remote.OutputPaths) == 0 {
		return task.ErrIdentityMismatch
	}
	return nil
}
