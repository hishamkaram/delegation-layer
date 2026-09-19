package taskdir

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// OpenBriefForExecution is the sole execution-only stdin capability. Its kernel
// descriptor is O_RDONLY and points to the exact validated immutable brief.
func (td *TaskDir) OpenBriefForExecution() (*os.File, error) {
	_, req, _, _, _, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return nil, err
	}
	f, err := td.store.openFile(filepath.Join(td.Dir, "brief.md"), os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err == nil && (n != req.BriefLength || hex.EncodeToString(h.Sum(nil)) != req.BriefSHA256) {
		err = task.ErrIdentityMismatch
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func (td *TaskDir) ReadSubmission() (*task.SubmitRecord, error) {
	_, _, meta, spec, hash, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return nil, err
	}
	var r task.SubmitRecord
	if err = td.store.readRecord(filepath.Join(td.Dir, "submit.json"), &r); err != nil {
		return nil, err
	}
	if err = task.ValidateSubmitRecord(&r); err != nil {
		return nil, err
	}
	if r.RootID != td.store.RootID || r.TaskID != td.TaskID || r.SpecSHA256 != spec || r.MetaSHA256 != hash || r.Supervisor != meta.SupervisorConfig {
		return nil, task.ErrIdentityMismatch
	}
	return &r, nil
}

func (td *TaskDir) ReadSupervisorReceipt() (*task.SupervisorReceipt, error) {
	submit, err := td.ReadSubmission()
	if err != nil {
		return nil, err
	}
	var r task.SupervisorReceipt
	if err = td.store.readRecord(filepath.Join(td.Dir, "supervisor.ref.json"), &r); err != nil {
		return nil, err
	}
	if err = matchSupervisorReceipt(&r, submit); err != nil {
		return nil, err
	}
	return &r, nil
}

func matchSupervisorReceipt(r *task.SupervisorReceipt, submit *task.SubmitRecord) error {
	if err := task.ValidateSupervisorReceipt(r); err != nil {
		return err
	}
	if r.RootID != submit.RootID || r.TaskID != submit.TaskID || r.SpecSHA256 != submit.SpecSHA256 || r.MetaSHA256 != submit.MetaSHA256 || r.Label != submit.Label || r.SupervisorRef() != submit.Supervisor {
		return task.ErrIdentityMismatch
	}
	return nil
}

func (td *TaskDir) RecordSupervisorReceipt(r task.SupervisorReceipt) (resultErr error) {
	if err := td.store.maintLock.LockSHNonblocking(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, td.store.maintLock.Unlock()) }()
	submit, err := td.ReadSubmission()
	if err != nil {
		return err
	}
	if err = matchSupervisorReceipt(&r, submit); err != nil {
		return err
	}
	return td.recordSame("supervisor.ref.json", r, func() (bool, error) {
		old, e := td.ReadSupervisorReceipt()
		return e == nil && reflect.DeepEqual(old, &r), e
	})
}

func (td *TaskDir) RecordSupervisorReceiptContext(ctx context.Context, r task.SupervisorReceipt) (resultErr error) {
	if ctx == nil {
		return context.Canceled
	}
	if err := td.store.maintLock.LockSH(ctx); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, td.store.maintLock.Unlock()) }()
	submit, err := td.ReadSubmission()
	if err != nil {
		return err
	}
	if err = matchSupervisorReceipt(&r, submit); err != nil {
		return err
	}
	return td.recordSameContext(ctx, "supervisor.ref.json", r, func() (bool, error) {
		old, e := td.ReadSupervisorReceipt()
		return e == nil && reflect.DeepEqual(old, &r), e
	})
}

// ReadProviderIdentity validates the optional side record without requiring a
// current workspace or external supervisor. Terminal readback does not call it.
func (td *TaskDir) ReadProviderIdentity() (*task.ProviderRefRecord, error) {
	_, req, _, spec, meta, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return nil, err
	}
	var r task.ProviderRefRecord
	if err = td.store.readRecord(filepath.Join(td.Dir, "provider.ref.json"), &r); err != nil {
		return nil, err
	}
	if err = task.ValidateProviderRefRecord(&r); err != nil {
		return nil, err
	}
	if r.RootID != td.store.RootID || r.TaskID != td.TaskID || r.SpecSHA256 != spec || r.MetaSHA256 != meta || r.Provider != req.Provider {
		return nil, task.ErrIdentityMismatch
	}
	if req.PriorSession != nil && r.ConversationID != req.PriorSession.ConversationID {
		return nil, task.ErrIdentityMismatch
	}
	return &r, nil
}

// RecordProviderIdentity may run while raw streams remain open. It requires the
// same consumed runner owner and grants neither launch nor terminal authority.
func (td *TaskDir) RecordProviderIdentity(identity task.SessionIdentity) error {
	state, err := td.ownedRunner()
	if err != nil {
		return err
	}
	defer state.mu.Unlock()
	if state.sealed {
		return task.ErrEvidenceFault
	}
	if err = task.ValidateSessionIdentity(identity); err != nil {
		return err
	}
	_, req, _, spec, meta, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return err
	}
	if identity.Provider != req.Provider || (req.PriorSession != nil && req.PriorSession.ConversationID != identity.ConversationID) {
		return task.ErrIdentityMismatch
	}
	r := task.ProviderRefRecord{SchemaVersion: task.SchemaVersion, RootID: td.store.RootID, TaskID: td.TaskID, SpecSHA256: spec, MetaSHA256: meta, Provider: identity.Provider, ConversationID: identity.ConversationID, ObservedAt: timestamp()}
	if err = task.ValidateProviderRefRecord(&r); err != nil {
		return err
	}
	return td.recordSame("provider.ref.json", r, func() (bool, error) {
		old, e := td.ReadProviderIdentity()
		return e == nil && old.Provider == r.Provider && old.ConversationID == r.ConversationID, e
	})
}

// recordSame always validates an existing record before comparing it. It bounds
// a new serialization only after existing-first retry comparison.
func (td *TaskDir) recordSame(name string, record any, readSame func() (bool, error)) error {
	same, err := readSame()
	if err == nil {
		if !same {
			return task.ErrIdentityMismatch
		}
		return td.acknowledgeRecord(name)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := marshalControlRecord(record)
	if err != nil {
		return err
	}
	_, cleanup, err := td.store.stageAndCommit(filepath.Dir(filepath.Join(td.Dir, name)), filepath.Base(name), data, td.store.faultInjector)
	if errors.Is(err, os.ErrExist) {
		same, readErr := readSame()
		if readErr != nil {
			return errors.Join(readErr, cleanup)
		}
		if !same {
			return errors.Join(task.ErrIdentityMismatch, cleanup)
		}
		return errors.Join(td.acknowledgeRecord(name), cleanup)
	}
	return errors.Join(err, cleanup)
}

// recordSameContext is the cancellation-aware counterpart to recordSame. It
// checks the context before reading or staging, then delegates staging to the
// durable context path. Once staging starts, stageReaderContext retains
// ownership of barriers and cleanup even when cancellation is observed.
func (td *TaskDir) recordSameContext(ctx context.Context, name string, record any, readSame func() (bool, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	same, err := readSame()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err == nil {
		return td.acknowledgeSameContext(ctx, name, same)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	data, err := marshalControlRecord(record)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	dirPath := filepath.Dir(filepath.Join(td.Dir, name))
	filename := filepath.Base(name)
	_, cleanup, commitErr := td.store.stageReaderContext(ctx, dirPath, filename, bytes.NewReader(data), td.store.faultInjector)
	if errors.Is(commitErr, os.ErrExist) {
		return td.reconcileStagedWinnerContext(ctx, name, cleanup, readSame)
	}
	return errors.Join(commitErr, cleanup)
}

func (td *TaskDir) acknowledgeSameContext(ctx context.Context, name string, same bool) error {
	if !same {
		return task.ErrIdentityMismatch
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return td.acknowledgeRecord(name)
}

func (td *TaskDir) reconcileStagedWinnerContext(ctx context.Context, name string, cleanup error, readSame func() (bool, error)) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(cleanup, err)
	}
	same, readErr := readSame()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(cleanup, ctxErr)
	}
	if readErr != nil {
		return errors.Join(readErr, cleanup)
	}
	if !same {
		return errors.Join(task.ErrIdentityMismatch, cleanup)
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(cleanup, err)
	}
	return errors.Join(td.acknowledgeRecord(name), cleanup)
}
