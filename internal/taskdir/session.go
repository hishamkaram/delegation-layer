package taskdir

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func (td *TaskDir) sessionDirectory(provider, conversation string) string {
	return filepath.Join(td.store.Root, "sessions", task.ComputeSHA256([]byte(provider+":"+conversation)))
}

func (td *TaskDir) withSession(provider, conversation string, fn func(string) error) (resultErr error) {
	return td.withSessionContext(context.Background(), provider, conversation, fn)
}

func (td *TaskDir) withSessionContext(ctx context.Context, provider, conversation string, fn func(string) error) (resultErr error) {
	if err := lifetimeContextErr(ctx); err != nil {
		return err
	}
	if err := td.store.maintLock.LockSHNonblockingContext(ctx); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, td.store.maintLock.Unlock()) }()
	dir := td.sessionDirectory(provider, conversation)
	if err := td.store.mkdirContext(ctx, dir, td.store.faultInjector); err != nil {
		return err
	}
	lock, err := td.openSessionLock(dir)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	// This lock protects short metadata mutations, not the active provider turn.
	// A runner and collector may release the same claim concurrently. Serialize
	// those mutations; the claim records still reject a genuinely active owner.
	acquisition, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err = lock.LockEX(acquisition); err != nil {
		return fmt.Errorf("%w: %w", task.ErrSessionBusy, err)
	}
	return fn(dir)
}

func (td *TaskDir) readClaim(dir, id, provider, conversation string) (*task.SessionClaimRecord, error) {
	var claim task.SessionClaimRecord
	if err := td.store.readRecord(filepath.Join(dir, id+".claim.json"), &claim); err != nil {
		return nil, err
	}
	if err := task.ValidateSessionClaimRecord(&claim); err != nil {
		return nil, err
	}
	if claim.TaskID != id || claim.RootID != td.store.RootID || claim.Provider != provider || claim.ConversationID != conversation {
		return nil, task.ErrIdentityMismatch
	}
	return &claim, nil
}

func (td *TaskDir) readReleaseContext(ctx context.Context, dir string, claim *task.SessionClaimRecord) (*task.SessionReleaseRecord, error) {
	var release task.SessionReleaseRecord
	if err := td.store.readRecord(filepath.Join(dir, claim.TaskID+".release.json"), &release); err != nil {
		return nil, err
	}
	if err := task.ValidateSessionReleaseRecord(&release); err != nil {
		return nil, err
	}
	if release.TaskID != claim.TaskID || release.RootID != claim.RootID || release.Provider != claim.Provider || release.ConversationID != claim.ConversationID {
		return nil, task.ErrIdentityMismatch
	}
	owner, err := td.store.OpenTask(claim.TaskID)
	if err != nil {
		return nil, err
	}
	defer closeTaskQuietly(owner)
	if err = owner.terminalProofContext(ctx, release.PredecessorEvidenceSHA256); err != nil {
		return nil, err
	}
	return &release, nil
}

func (td *TaskDir) activeSessionOwnerContext(ctx context.Context, dir, provider, conversation string) (string, error) {
	f, err := td.store.openDir(dir)
	if err != nil {
		return "", err
	}
	entries, err := f.ReadDir(-1)
	err = errors.Join(err, f.Close())
	if err != nil {
		return "", err
	}
	owner := ""
	if err = td.validateReleaseNames(dir, provider, conversation, entries); err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".claim.json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".claim.json")
		claim, e := td.readClaim(dir, id, provider, conversation)
		if e != nil {
			return "", e
		}
		_, e = td.readReleaseContext(ctx, dir, claim)
		if e == nil {
			continue
		}
		if !errors.Is(e, os.ErrNotExist) {
			return "", e
		}
		if owner != "" {
			return "", task.ErrInvariantFault
		}
		owner = id
	}
	return owner, nil
}

func (td *TaskDir) ClaimSession(provider, conversation string) error {
	return td.ClaimSessionContext(context.Background(), provider, conversation)
}

func (td *TaskDir) ClaimSessionContext(ctx context.Context, provider, conversation string) error {
	if err := lifetimeContextErr(ctx); err != nil {
		return err
	}

	rec := task.SessionClaimRecord{SchemaVersion: task.SchemaVersion, RootID: td.store.RootID, TaskID: td.TaskID, Provider: provider, ConversationID: conversation, ClaimedAt: timestamp()}
	if err := task.ValidateSessionClaimRecord(&rec); err != nil {
		return err
	}
	req, _, err := td.PreparedRecords()
	if err != nil {
		return err
	}
	if req.Provider != provider || (req.PriorSession != nil && req.PriorSession.ConversationID != conversation) {
		return task.ErrIdentityMismatch
	}
	return td.withSessionContext(ctx, provider, conversation, func(dir string) error {
		return td.claimSessionRecordContext(ctx, dir, rec)
	})
}

func (td *TaskDir) terminalProofContext(ctx context.Context, digest string) (resultErr error) {
	if err := td.runLock.LockEXNonblockingContext(ctx); err != nil {
		return &sessionProofLockError{cause: err}
	}
	defer func() { resultErr = errors.Join(resultErr, td.runLock.Unlock()) }()
	_, meta, spec, metaHash, err := td.loadAndValidatePreparedSetContext(ctx)
	if err != nil {
		return err
	}
	winner, err := td.readWinner(meta, spec, metaHash)
	if err == nil {
		if winner.EvidenceSHA256 == digest || winner.Payload.SHA256 == digest {
			return td.acknowledgeRecord("outcome.json")
		}
		// A handoff release is a valid proof even when the provider publishes
		// its terminal outcome after the release. Keep the recorded handoff
		// valid while outcome.json remains the terminal authority.
		if matches, matchErr := td.matchesHandoffProofContext(ctx, digest); matchErr != nil {
			return matchErr
		} else if matches {
			return td.acknowledgeRecord("outcome.json")
		}
		return task.ErrEvidenceFault
	}
	if !errors.Is(err, errNoOutcome) {
		return err
	}
	exists, err := td.store.exists(filepath.Join(td.Dir, "outcome.json"))
	if err != nil {
		return err
	}
	if exists {
		return task.ErrEvidenceFault
	}
	if matches, matchErr := td.matchesHandoffProofContext(ctx, digest); matchErr != nil {
		return matchErr
	} else if matches {
		return nil
	}
	seal, err := td.readSeal(meta.Predicate, spec, metaHash)
	if err != nil {
		return fmt.Errorf("%w: %w", task.ErrSessionBusy, err)
	}
	if seal.ManifestSHA256 == digest {
		return td.acknowledgeRecord("provider.exit")
	}
	return task.ErrSessionBusy
}

// sessionProofLockError labels a failed acquisition without adding active
// session ownership as an independent cause. A plain ErrSessionBusy still means
// the claim/proof rules refused release, rather than temporary lock contention.
type sessionProofLockError struct{ cause error }

func (e *sessionProofLockError) Error() string {
	return fmt.Sprintf("%s: active runner: %v", task.ErrSessionBusy, e.cause)
}

func (e *sessionProofLockError) Is(target error) bool {
	return target == task.ErrSessionBusy
}

func (e *sessionProofLockError) Unwrap() error { return e.cause }

// ReleaseSessionAfterTimeout transfers a continuation claim after a budget
// stop has a durable ended observation but before the provider can publish an
// outcome. The stop observation is the handoff proof; outcome.json remains the
// task-completion authority even if provider.exit was sealed in the crash
// window before publication.
func (td *TaskDir) ReleaseSessionAfterTimeout(provider, conversation string) error {
	return td.ReleaseSessionAfterTimeoutContext(context.Background(), provider, conversation)
}

func (td *TaskDir) ReleaseSessionAfterTimeoutContext(ctx context.Context, provider, conversation string) error {
	if err := lifetimeContextErr(ctx); err != nil {
		return err
	}

	digest, err := td.timeoutEvidenceDigest()
	if err != nil {
		return err
	}
	return td.ReleaseSessionContext(ctx, provider, conversation, digest)
}

func (td *TaskDir) ReleaseSession(provider, conversation, digest string) error {
	return td.ReleaseSessionContext(context.Background(), provider, conversation, digest)
}

func (td *TaskDir) ReleaseSessionContext(ctx context.Context, provider, conversation, digest string) error {
	if err := lifetimeContextErr(ctx); err != nil {
		return err
	}

	rec := task.SessionReleaseRecord{SchemaVersion: task.SchemaVersion, RootID: td.store.RootID, TaskID: td.TaskID, Provider: provider, ConversationID: conversation, PredecessorEvidenceSHA256: digest, ReleasedAt: timestamp()}
	if err := task.ValidateSessionReleaseRecord(&rec); err != nil {
		return err
	}
	return td.withSessionContext(ctx, provider, conversation, func(dir string) error {
		return td.releaseSessionRecordContext(ctx, dir, rec)
	})
}

func (td *TaskDir) releaseSessionRecordContext(ctx context.Context, dir string, rec task.SessionReleaseRecord) error {
	claim, err := td.readClaim(dir, rec.TaskID, rec.Provider, rec.ConversationID)
	if err != nil {
		return err
	}
	existing, err := td.readReleaseContext(ctx, dir, claim)
	if err == nil {
		return td.reconcileSessionReleaseContext(ctx, dir, existing, rec.PredecessorEvidenceSHA256)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	owner, err := td.activeSessionOwnerContext(ctx, dir, rec.Provider, rec.ConversationID)
	if err != nil {
		return err
	}
	if owner != rec.TaskID {
		return task.ErrSessionBusy
	}
	if err := td.terminalProofContext(ctx, rec.PredecessorEvidenceSHA256); err != nil {
		return err
	}
	return td.commitSessionReleaseContext(ctx, dir, rec)
}

func (td *TaskDir) reconcileSessionReleaseContext(ctx context.Context, dir string, existing *task.SessionReleaseRecord, digest string) error {
	if existing.PredecessorEvidenceSHA256 == digest {
		return td.store.barrierDir(dir)
	}
	if matches, matchErr := td.matchesHandoffProofContext(ctx, existing.PredecessorEvidenceSHA256); matchErr != nil {
		return matchErr
	} else if !matches {
		return task.ErrEvidenceFault
	}
	// A handoff can be recorded before the provider publishes its
	// terminal outcome, or before another compatible witness appears.
	// Accept the later digest as the same completed release after
	// validating it independently.
	if err := td.terminalProofContext(ctx, digest); err != nil {
		return err
	}
	return td.store.barrierDir(dir)
}

func (td *TaskDir) commitSessionReleaseContext(ctx context.Context, dir string, rec task.SessionReleaseRecord) error {
	data, err := marshalControlRecord(rec)
	if err != nil {
		return err
	}
	_, cleanup, err := td.store.stageReaderContext(ctx, dir, rec.TaskID+".release.json", bytes.NewReader(data), td.store.faultInjector)
	return errors.Join(err, cleanup)
}

func (td *TaskDir) validateReleaseNames(dir, provider, conversation string, entries []os.DirEntry) error {
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".release.json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".release.json")
		if _, err := td.readClaim(dir, id, provider, conversation); err != nil {
			return fmt.Errorf("%w: release without valid claim: %w", task.ErrEvidenceFault, err)
		}
	}
	return nil
}

func (td *TaskDir) openSessionLock(dir string) (*LockFile, error) {
	f, err := td.store.openDir(dir)
	if err != nil {
		return nil, err
	}
	entries, err := f.ReadDir(1)
	closeErr := f.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, errors.Join(err, closeErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(entries) == 0 {
		return td.store.openLock(filepath.Join(dir, ".session.lock"), LockLevelSession)
	}
	return td.store.openExistingLock(filepath.Join(dir, ".session.lock"), LockLevelSession)
}

func (td *TaskDir) claimSessionRecordContext(ctx context.Context, dir string, rec task.SessionClaimRecord) error {
	provider, conversation := rec.Provider, rec.ConversationID

	owner, e := td.activeSessionOwnerContext(ctx, dir, provider, conversation)
	if e != nil {
		return e
	}
	if owner == td.TaskID {
		return td.store.barrierDir(dir)
	}
	if owner != "" {
		return task.ErrSessionBusy
	}
	old, e := td.readClaim(dir, td.TaskID, provider, conversation)
	if e == nil {
		// A released task cannot reclaim its predecessor's session.
		if _, e = td.readReleaseContext(ctx, dir, old); e == nil {
			return task.ErrSessionBusy
		}
		return e
	}
	if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	data, e := marshalControlRecord(rec)
	if e != nil {
		return e
	}
	_, cleanup, e := td.store.stageReaderContext(ctx, dir, td.TaskID+".claim.json", bytes.NewReader(data), td.store.faultInjector)
	return errors.Join(e, cleanup)
}
