package taskdir

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func (td *TaskDir) sessionDirectory(provider, conversation string) string {
	return filepath.Join(td.store.Root, "sessions", task.ComputeSHA256([]byte(provider+":"+conversation)))
}

func (td *TaskDir) withSession(provider, conversation string, fn func(string) error) (resultErr error) {
	if err := td.store.maintLock.LockSHNonblocking(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, td.store.maintLock.Unlock()) }()
	dir := td.sessionDirectory(provider, conversation)
	if err := td.store.mkdir(dir, td.store.faultInjector); err != nil {
		return err
	}
	lock, err := td.openSessionLock(dir)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	if err = lock.LockEXNonblocking(); err != nil {
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

func (td *TaskDir) readRelease(dir string, claim *task.SessionClaimRecord) (*task.SessionReleaseRecord, error) {
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
	if err = owner.terminalProof(release.PredecessorEvidenceSHA256); err != nil {
		return nil, err
	}
	return &release, nil
}

func (td *TaskDir) activeSessionOwner(dir, provider, conversation string) (string, error) {
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
		_, e = td.readRelease(dir, claim)
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
	return td.withSession(provider, conversation, func(dir string) error {
		owner, e := td.activeSessionOwner(dir, provider, conversation)
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
			if _, e = td.readRelease(dir, old); e == nil {
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
		_, cleanup, e := td.store.stageAndCommit(dir, td.TaskID+".claim.json", data, td.store.faultInjector)
		return errors.Join(e, cleanup)
	})
}

func (td *TaskDir) terminalProof(digest string) (resultErr error) {
	if err := td.runLock.LockEXNonblocking(); err != nil {
		return fmt.Errorf("%w: active runner: %w", task.ErrSessionBusy, err)
	}
	defer func() { resultErr = errors.Join(resultErr, td.runLock.Unlock()) }()
	_, _, meta, spec, metaHash, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return err
	}
	winner, err := td.readWinner(meta, spec, metaHash)
	if err == nil {
		if winner.EvidenceSHA256 == digest || winner.Payload.SHA256 == digest {
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
	seal, err := td.readSeal(meta.Predicate, spec, metaHash)
	if err != nil {
		return fmt.Errorf("%w: %w", task.ErrSessionBusy, err)
	}
	if seal.ManifestSHA256 == digest {
		return td.acknowledgeRecord("provider.exit")
	}
	return task.ErrSessionBusy
}

func (td *TaskDir) ReleaseSession(provider, conversation, digest string) error {
	rec := task.SessionReleaseRecord{SchemaVersion: task.SchemaVersion, RootID: td.store.RootID, TaskID: td.TaskID, Provider: provider, ConversationID: conversation, PredecessorEvidenceSHA256: digest, ReleasedAt: timestamp()}
	if err := task.ValidateSessionReleaseRecord(&rec); err != nil {
		return err
	}
	return td.withSession(provider, conversation, func(dir string) error {
		claim, e := td.readClaim(dir, td.TaskID, provider, conversation)
		if e != nil {
			return e
		}
		existing, e := td.readRelease(dir, claim)
		if e == nil {
			if existing.PredecessorEvidenceSHA256 != digest {
				return task.ErrEvidenceFault
			}
			return td.store.barrierDir(dir)
		}
		if !errors.Is(e, os.ErrNotExist) {
			return e
		}
		owner, e := td.activeSessionOwner(dir, provider, conversation)
		if e != nil {
			return e
		}
		if owner != td.TaskID {
			return task.ErrSessionBusy
		}
		if e = td.terminalProof(digest); e != nil {
			return e
		}
		data, e := marshalControlRecord(rec)
		if e != nil {
			return e
		}
		_, cleanup, e := td.store.stageAndCommit(dir, td.TaskID+".release.json", data, td.store.faultInjector)
		return errors.Join(e, cleanup)
	})
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
