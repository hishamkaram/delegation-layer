package taskdir

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

// TaskDir owns independent lock descriptors. Execution authority is never
// reconstructed from a pathname or transferred to another opened TaskDir.
type TaskDir struct {
	store          *Store
	TaskID         string
	Dir            string
	admissionLock  *LockFile
	runLock        *LockFile
	mu             sync.Mutex
	runnerLease    *RunnerLease
	admissionLease *AdmissionLease
	closed         bool
}

func closeTaskQuietly(td *TaskDir) {
	if td != nil {
		if err := td.Close(); err != nil {
			return
		}
	}
}

func unlockLockQuietly(l *LockFile) {
	if l != nil {
		if err := l.Unlock(); err != nil {
			return
		}
	}
}
func timestamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (s *Store) newTaskHandle(id string, createLocks bool) (*TaskDir, error) {
	openLock := s.openExistingLock
	if createLocks {
		openLock = s.openLock
	}
	dir := filepath.Join(s.Root, "tasks", id)
	adm, err := openLock(filepath.Join(dir, ".admission.lock"), LockLevelAdmission)
	if err != nil {
		return nil, err
	}
	run, err := openLock(filepath.Join(dir, ".run.lock"), LockLevelRun)
	if err != nil {
		closeLockQuietly(adm)
		return nil, err
	}
	return &TaskDir{store: s, TaskID: id, Dir: dir, admissionLock: adm, runLock: run}, nil
}

func (s *Store) OpenTask(id string) (*TaskDir, error) {
	if err := task.ValidateTaskID(id); err != nil {
		return nil, err
	}
	return s.newTaskHandle(id, false)
}

func (td *TaskDir) Close() error {
	td.mu.Lock()
	defer td.mu.Unlock()
	if td.closed {
		return nil
	}
	if td.runnerLease != nil {
		if err := td.runnerLease.Release(); err != nil {
			return err
		}
	}
	if td.admissionLease != nil {
		if err := td.admissionLease.Release(); err != nil {
			return err
		}
	}
	if err := td.runLock.closeIdle(); err != nil {
		return err
	}
	if err := td.admissionLock.closeIdle(); err != nil {
		return err
	}
	td.closed = true
	return nil
}

func (s *Store) CreateTask(id string, req *task.TaskRecord, brief []byte, meta *task.MetaRecord) (_ *TaskDir, resultErr error) {
	reqData, metaData, err := s.validateCreateInput(id, req, brief, meta)
	if err != nil {
		return nil, err
	}
	if err = s.maintLock.LockSHNonblocking(); err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, s.maintLock.Unlock()) }()
	dir := filepath.Join(s.Root, "tasks", id)
	if err = s.mkdir(dir, s.faultInjector); err != nil {
		return nil, err
	}
	createLocks, err := s.allowInitialLocks(dir)
	if err != nil {
		return nil, err
	}
	td, err := s.newTaskHandle(id, createLocks)
	if err != nil {
		return nil, err
	}
	if err = td.admissionLock.LockEXNonblocking(); err != nil {
		closeTaskQuietly(td)
		return nil, err
	}
	err = td.prepareSet(brief, reqData, metaData)
	err = errors.Join(err, td.admissionLock.Unlock())
	if err != nil {
		closeTaskQuietly(td)
		return nil, err
	}
	return td, nil
}

func (s *Store) validateCreateInput(id string, req *task.TaskRecord, brief []byte, meta *task.MetaRecord) ([]byte, []byte, error) {
	if req == nil || meta == nil {
		return nil, nil, errors.New("nil prepared record")
	}
	if err := NormalizeTaskRecord(s.Root, req); err != nil {
		return nil, nil, err
	}
	if err := task.ValidateTaskID(id); err != nil {
		return nil, nil, err
	}
	if err := task.ValidateTaskRecord(req); err != nil {
		return nil, nil, err
	}
	if !preparedIDsMatch(s.RootID, id, req, meta) {
		return nil, nil, task.ErrIdentityMismatch
	}
	if int64(len(brief)) != req.BriefLength || task.ComputeSHA256(brief) != req.BriefSHA256 {
		return nil, nil, task.ErrIdentityMismatch
	}
	reqData, err := marshalControlRecord(req)
	if err != nil {
		return nil, nil, err
	}
	meta.SpecSHA256 = task.ComputeSHA256(reqData)
	if err = task.ValidateMetaRecord(meta); err != nil {
		return nil, nil, err
	}
	if meta.Predicate.Adapter != req.Provider || meta.Predicate.Mode != req.Mode || !reflect.DeepEqual(meta.RequestedConfig, req.RequestedConfig) {
		return nil, nil, task.ErrIdentityMismatch
	}
	metaData, err := s.marshalPreparedMeta(id, meta)
	if err != nil {
		return nil, nil, err
	}
	return reqData, metaData, nil
}

func (td *TaskDir) prepareSet(brief, reqData, metaData []byte) error {
	guarded := false
	for _, name := range []string{"submit.json", "provider.start"} {
		found, err := td.store.exists(filepath.Join(td.Dir, name))
		if err != nil {
			return err
		}
		guarded = guarded || found
	}
	records := []struct {
		name   string
		data   []byte
		exists bool
	}{{"brief.md", brief, false}, {"task.json", reqData, false}, {"meta.json", metaData, false}}
	// Validate all available fragments before adding any missing immutable record.
	for index, record := range records {
		exists, err := td.validatePreparedRecord(record.name, record.data, guarded)
		if err != nil {
			return err
		}
		records[index].exists = exists
	}
	for _, record := range records {
		if record.exists {
			continue
		}
		_, cleanupErr, err := td.store.stageAndCommit(td.Dir, record.name, record.data, td.store.faultInjector)
		if err = errors.Join(err, cleanupErr); err != nil {
			return err
		}
	}
	_, _, _, _, _, err := td.loadAndValidatePreparedSet()
	return err
}

func (td *TaskDir) validatePreparedRecord(name string, data []byte, guarded bool) (bool, error) {
	limit := int64(task.MaxControlRecordSize)
	if name == "brief.md" {
		limit = task.MaxBriefSize
	}
	existing, err := td.store.readBytes(filepath.Join(td.Dir, name), limit)
	if err == nil {
		matches, e := samePreparedRecord(name, existing, data)
		if e != nil {
			return false, e
		}
		if !matches {
			return false, task.ErrRequestConflict
		}
		return true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if guarded {
		return false, fmt.Errorf("%w: guarded prepared set is incomplete", task.ErrInvariantFault)
	}
	return false, nil
}

func samePreparedRecord(name string, existing, data []byte) (bool, error) {
	if name != "meta.json" {
		return bytes.Equal(existing, data), nil
	}
	var old, proposed task.MetaRecord
	if err := task.DecodeStrict(existing, &old); err != nil {
		return false, err
	}
	if err := task.DecodeStrict(data, &proposed); err != nil {
		return false, err
	}
	if err := task.ValidateMetaRecord(&old); err != nil {
		return false, err
	}
	proposed.CreatedAt = old.CreatedAt
	return reflect.DeepEqual(old, proposed), nil
}

func (td *TaskDir) loadAndValidatePreparedSet() ([]byte, *task.TaskRecord, *task.MetaRecord, string, string, error) {
	brief, err := td.store.readBytes(filepath.Join(td.Dir, "brief.md"), task.MaxBriefSize)
	if err != nil {
		return nil, nil, nil, "", "", err
	}
	reqBytes, err := td.store.readBytes(filepath.Join(td.Dir, "task.json"), task.MaxControlRecordSize)
	if err != nil {
		return nil, nil, nil, "", "", err
	}
	metaBytes, err := td.store.readBytes(filepath.Join(td.Dir, "meta.json"), task.MaxControlRecordSize)
	if err != nil {
		return nil, nil, nil, "", "", err
	}
	var req task.TaskRecord
	var meta task.MetaRecord
	if err = task.DecodeStrict(reqBytes, &req); err != nil {
		return nil, nil, nil, "", "", err
	}
	if err = task.ValidateTaskRecord(&req); err != nil {
		return nil, nil, nil, "", "", err
	}
	if err = task.DecodeStrict(metaBytes, &meta); err != nil {
		return nil, nil, nil, "", "", err
	}
	if err = task.ValidateMetaRecord(&meta); err != nil {
		return nil, nil, nil, "", "", err
	}
	specHash := task.ComputeSHA256(reqBytes)
	metaHash := task.ComputeSHA256(metaBytes)
	if err = td.matchPrepared(&req, &meta, brief, specHash); err != nil {
		return nil, nil, nil, "", "", err
	}
	if err = td.validateGuards(&req, &meta, specHash, metaHash); err != nil {
		return nil, nil, nil, "", "", err
	}
	return brief, &req, &meta, specHash, metaHash, nil
}

func (td *TaskDir) matchPrepared(req *task.TaskRecord, meta *task.MetaRecord, brief []byte, spec string) error {
	if req.RootID != td.store.RootID || req.TaskID != td.TaskID || meta.RootID != td.store.RootID || meta.TaskID != td.TaskID || meta.SpecSHA256 != spec {
		return task.ErrIdentityMismatch
	}
	if req.BriefSHA256 != task.ComputeSHA256(brief) || req.BriefLength != int64(len(brief)) {
		return task.ErrIdentityMismatch
	}
	if meta.Predicate.Adapter != req.Provider || meta.Predicate.Mode != req.Mode || !reflect.DeepEqual(req.RequestedConfig, meta.RequestedConfig) {
		return task.ErrIdentityMismatch
	}
	return nil
}

func (td *TaskDir) validateGuards(req *task.TaskRecord, meta *task.MetaRecord, specHash, metaHash string) error {
	if err := td.validateSubmitGuard(meta, specHash, metaHash); err != nil {
		return err
	}
	return td.validateStartGuard(req, specHash, metaHash)
}

func (td *TaskDir) validateSubmitGuard(meta *task.MetaRecord, specHash, metaHash string) error {
	var submit task.SubmitRecord
	if err := td.store.readRecord(filepath.Join(td.Dir, "submit.json"), &submit); err == nil {
		if err = task.ValidateSubmitRecord(&submit); err != nil {
			return err
		}
		if submit.RootID != td.store.RootID || submit.TaskID != td.TaskID || submit.SpecSHA256 != specHash || submit.MetaSHA256 != metaHash || !reflect.DeepEqual(submit.Supervisor, meta.SupervisorConfig) {
			return task.ErrIdentityMismatch
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (td *TaskDir) validateStartGuard(req *task.TaskRecord, specHash, metaHash string) error {
	var start task.ProviderStartRecord
	if err := td.store.readRecord(filepath.Join(td.Dir, "provider.start"), &start); err == nil {
		if err = task.ValidateProviderStartRecord(&start); err != nil {
			return err
		}
		if start.RootID != td.store.RootID || start.TaskID != td.TaskID || start.SpecSHA256 != specHash || start.MetaSHA256 != metaHash || start.BudgetNanos != req.BudgetNanos {
			return task.ErrIdentityMismatch
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// PreparedRecords returns the complete validated immutable request and binding.
func (td *TaskDir) PreparedRecords() (*task.TaskRecord, *task.MetaRecord, error) {
	_, req, meta, _, _, err := td.loadAndValidatePreparedSet()
	return req, meta, err
}

func (td *TaskDir) PrepareSubmission(supervisor task.SupervisorRef) (_ *SubmissionPermit, resultErr error) {
	if err := td.store.maintLock.LockSHNonblocking(); err != nil {
		return nil, err
	}
	if err := td.admissionLock.LockEXNonblocking(); err != nil {
		return nil, errors.Join(err, td.store.maintLock.Unlock())
	}
	success := false
	defer func() {
		if !success {
			resultErr = errors.Join(resultErr, td.admissionLock.Unlock(), td.store.maintLock.Unlock())
		}
	}()
	_, req, meta, specHash, metaHash, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(supervisor, meta.SupervisorConfig) {
		return nil, task.ErrIdentityMismatch
	}
	exists, err := td.store.exists(filepath.Join(td.Dir, "submit.json"))
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, task.ErrAlreadySubmitted
	}
	if err = td.validateExecutionDirectories(req); err != nil {
		return nil, err
	}
	rec := task.SubmitRecord{SchemaVersion: task.SchemaVersion, RootID: td.store.RootID, TaskID: td.TaskID, SpecSHA256: specHash, MetaSHA256: metaHash, Label: fmt.Sprintf("delegate:%s:%s", td.store.RootID, td.TaskID), Supervisor: supervisor, CreatedAt: timestamp()}
	if err = task.ValidateSubmitRecord(&rec); err != nil {
		return nil, err
	}
	if err = td.commitRecord("submit.json", rec); err != nil {
		return nil, err
	}
	lease := newAdmissionLease(td.admissionLock, td.store.maintLock)
	td.mu.Lock()
	td.admissionLease = lease
	td.mu.Unlock()
	success = true
	return newSubmissionPermit(td.TaskID, lease), nil
}

func (td *TaskDir) commitRecord(name string, rec any) error {
	data, err := marshalControlRecord(rec)
	if err != nil {
		return err
	}
	_, cleanupErr, err := td.store.stageAndCommit(td.Dir, name, data, td.store.faultInjector)
	return errors.Join(err, cleanupErr)
}

func (td *TaskDir) PrepareStart(budget int64) (_ *StartPermit, resultErr error) {
	if err := td.store.maintLock.LockSHNonblocking(); err != nil {
		return nil, err
	}
	if err := td.runLock.LockEXNonblocking(); err != nil {
		return nil, errors.Join(err, td.store.maintLock.Unlock())
	}
	success := false
	defer func() {
		if !success {
			resultErr = errors.Join(resultErr, td.runLock.Unlock(), td.store.maintLock.Unlock())
		}
	}()
	_, req, _, specHash, metaHash, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return nil, err
	}
	if budget == 0 {
		budget = req.BudgetNanos
	}
	if budget != req.BudgetNanos {
		return nil, task.ErrRequestConflict
	}
	for _, name := range []string{"provider.start", "provider.exit", "outcome.json"} {
		exists, e := td.store.exists(filepath.Join(td.Dir, name))
		if e != nil {
			return nil, e
		}
		if exists {
			return nil, task.ErrAlreadyStarted
		}
	}
	if err = td.validateExecutionDirectories(req); err != nil {
		return nil, err
	}
	if err = td.initializeRaw(); err != nil {
		return nil, err
	}
	rec := task.ProviderStartRecord{SchemaVersion: task.SchemaVersion, RootID: td.store.RootID, TaskID: td.TaskID, SpecSHA256: specHash, MetaSHA256: metaHash, BudgetNanos: budget, CreatedAt: timestamp()}
	if err = task.ValidateProviderStartRecord(&rec); err != nil {
		return nil, err
	}
	if err = td.commitRecord("provider.start", rec); err != nil {
		return nil, err
	}
	lease := newRunnerLease(td.runLock, td.store.maintLock, filepath.Join(td.Dir, "raw"))
	td.mu.Lock()
	td.runnerLease = lease
	td.mu.Unlock()
	success = true
	return newStartPermit(td.TaskID, lease), nil
}

func (td *TaskDir) initializeRaw() error {
	dir := filepath.Join(td.Dir, "raw")
	if err := td.store.mkdir(dir, td.store.faultInjector); err != nil {
		return err
	}
	for _, name := range []string{"stdout", "stderr"} {
		f, err := td.store.openFile(filepath.Join(dir, name), unix.O_CREAT|unix.O_RDWR)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil {
			closeFileQuietly(f)
			return err
		}
		if info.Size() != 0 {
			closeFileQuietly(f)
			return fmt.Errorf("%w: unexplained pre-start raw bytes", task.ErrEvidenceFault)
		}
		if err = errors.Join(platformBarrierFile(f), f.Close()); err != nil {
			return err
		}
	}
	return td.store.barrierDir(dir)
}

func preparedIDsMatch(root, id string, req *task.TaskRecord, meta *task.MetaRecord) bool {
	return req.RootID == root && req.TaskID == id && meta.RootID == root && meta.TaskID == id
}

func (s *Store) allowInitialLocks(dir string) (bool, error) {
	for _, name := range []string{"brief.md", "task.json", "meta.json", "submit.json", "provider.start"} {
		exists, err := s.entryExists(filepath.Join(dir, name))
		if err != nil {
			return false, err
		}
		if exists {
			return false, nil
		}
	}
	return true, nil
}
