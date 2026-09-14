package taskdir

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type (
	// StopRecord is one validated durable stop request and its optional
	// supervisor response and termination observation. A nil optional field is
	// an unknown/unrecorded fact, not a negative observation.
	StopRecord struct {
		Request     *task.StopRequestRecord
		Reply       *task.StopReplyRecord
		Observation *task.StopObservedRecord
	}
	stopState struct {
		mu                 sync.Mutex
		maintenance        *LockFile
		request            task.StopRequestRecord
		consumed, released bool
		releaseErr         error
	}
	stopRecordIDs struct {
		requests, replies, observations map[string]struct{}
		entries                         int
	}
	// StopPermit is granted only to the newly durable request creator. Copies share
	// private one-use state; saved requests and receipts cannot reconstruct it.
	StopPermit struct{ state *stopState }
)

const (
	stopRequestSuffix  = ".request.json"
	stopReplySuffix    = ".reply.json"
	stopObservedSuffix = ".observed.json"

	// MaxStopRecords bounds one ReadStopRecords inspection. It does not limit
	// stop request creation or remove records; callers receive
	// ErrStopInspectionLimit when a task has more durable requests.
	MaxStopRecords = 64
	// MaxStopInspectionBytes bounds the total encoded request, reply, and
	// observation bytes loaded by one ReadStopRecords inspection.
	MaxStopInspectionBytes = 8 * task.MaxControlRecordSize
	maxStopRecordEntries   = 3 * MaxStopRecords
)

// ErrStopInspectionLimit reports that a read-only stop inspection exceeded its
// bounded request count or encoded-byte budget.
var ErrStopInspectionLimit = errors.New("stop inspection limit exceeded")

func (p *StopPermit) TaskID() string {
	if p == nil || p.state == nil {
		return ""
	}
	return p.state.request.TaskID
}

func (p *StopPermit) RequestID() string {
	if p == nil || p.state == nil {
		return ""
	}
	return p.state.request.RequestID
}

func (p *StopPermit) Request() (*task.StopRequestRecord, error) {
	if p == nil || p.state == nil {
		return nil, task.ErrInvalidPermit
	}
	s := p.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return nil, task.ErrInvalidPermit
	}
	r := s.request
	if r.NumericTaskID != nil {
		id := *r.NumericTaskID
		r.NumericTaskID = &id
	}
	return &r, nil
}

func (p *StopPermit) Consume() error {
	if p == nil || p.state == nil {
		return task.ErrInvalidPermit
	}
	s := p.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return task.ErrInvalidPermit
	}
	if s.consumed {
		return task.ErrPermitAlreadyUsed
	}
	s.consumed = true
	return nil
}

func (p *StopPermit) Release() error {
	if p == nil || p.state == nil {
		return nil
	}
	s := p.state
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.released {
		s.released = true
		s.releaseErr = s.maintenance.Unlock()
	}
	return s.releaseErr
}

func (td *TaskDir) PrepareStop(requestID, cause string, deadline time.Time) (_ *StopPermit, resultErr error) {
	if err := task.ValidateStopRequestID(requestID); err != nil {
		return nil, err
	}
	if err := td.store.maintLock.LockSHNonblocking(); err != nil {
		return nil, err
	}
	granted := false
	defer func() {
		if !granted {
			resultErr = errors.Join(resultErr, td.store.maintLock.Unlock())
		}
	}()
	r, err := td.newStopRequest(requestID, cause, deadline)
	if err != nil {
		return nil, err
	}
	existing, err := td.ReadStopRequest(requestID)
	if err == nil {
		if existing.Cause != r.Cause || existing.Deadline != r.Deadline {
			return nil, task.ErrRequestConflict
		}
		return nil, task.ErrStopAlreadyRequested
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	data, err := marshalControlRecord(r)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(td.Dir, "stop")
	if err = td.store.mkdir(dir, td.store.faultInjector); err != nil {
		return nil, err
	}
	_, cleanup, err := td.store.stageAndCommit(dir, requestID+".request.json", data, td.store.faultInjector)
	if errors.Is(err, os.ErrExist) {
		return nil, errors.Join(task.ErrStopAlreadyRequested, cleanup)
	}
	if err = errors.Join(err, cleanup); err != nil {
		return nil, err
	}
	granted = true
	return &StopPermit{state: &stopState{maintenance: td.store.maintLock, request: *r}}, nil
}

func (td *TaskDir) newStopRequest(id, cause string, deadline time.Time) (*task.StopRequestRecord, error) {
	_, req, meta, spec, hash, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return nil, err
	}
	_, err = td.readWinner(meta, spec, hash)
	if err == nil {
		return nil, task.ErrTerminalTask
	}
	if !errors.Is(err, errNoOutcome) {
		return nil, err
	}
	submit, err := td.ReadSubmission()
	if err != nil {
		return nil, err
	}
	r := &task.StopRequestRecord{SchemaVersion: task.SchemaVersion, RootID: td.store.RootID, TaskID: td.TaskID, SpecSHA256: spec, MetaSHA256: hash, Label: submit.Label, Supervisor: submit.Supervisor, RequestID: id, Cause: cause, BudgetNanos: req.BudgetNanos, RequestedAt: timestamp()}
	receipt, receiptErr := td.ReadSupervisorReceipt()
	if receiptErr == nil {
		numeric := receipt.NumericTaskID
		r.NumericTaskID = &numeric
	} else if !errors.Is(receiptErr, os.ErrNotExist) {
		return nil, receiptErr
	}
	if !deadline.IsZero() {
		r.Deadline = deadline.UTC().Format(time.RFC3339Nano)
	}
	if err = task.ValidateStopRequestRecord(r); err != nil {
		return nil, err
	}
	if cause == "budget" {
		var start task.ProviderStartRecord
		if err = td.store.readRecord(filepath.Join(td.Dir, "provider.start"), &start); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (td *TaskDir) ReadStopRequest(id string) (*task.StopRequestRecord, error) {
	if err := task.ValidateStopRequestID(id); err != nil {
		return nil, err
	}
	var r task.StopRequestRecord
	if err := td.store.readRecord(filepath.Join(td.Dir, "stop", id+".request.json"), &r); err != nil {
		return nil, err
	}
	if err := task.ValidateStopRequestRecord(&r); err != nil {
		return nil, err
	}
	_, req, meta, spec, hash, err := td.loadAndValidatePreparedSet()
	if err != nil {
		return nil, err
	}
	if r.RequestID != id || r.RootID != td.store.RootID || r.TaskID != td.TaskID || r.SpecSHA256 != spec || r.MetaSHA256 != hash || r.Supervisor != meta.SupervisorConfig || r.BudgetNanos != req.BudgetNanos {
		return nil, task.ErrIdentityMismatch
	}
	if r.NumericTaskID != nil {
		receipt, e := td.ReadSupervisorReceipt()
		if e != nil {
			return nil, e
		}
		if *r.NumericTaskID != receipt.NumericTaskID {
			return nil, task.ErrIdentityMismatch
		}
	}
	return &r, nil
}

func (td *TaskDir) stopEvidence(id string) (*task.StopRequestRecord, string, *task.SupervisorReceipt, error) {
	r, err := td.ReadStopRequest(id)
	if err != nil {
		return nil, "", nil, err
	}
	if r.NumericTaskID == nil {
		// A request without an already-saved target can never acquire a
		// supervisor reply or termination observation. A later receipt belongs
		// to a separately authorized request and must not upgrade this one.
		return nil, "", nil, task.ErrIdentityMismatch
	}
	data, err := td.store.readBytes(filepath.Join(td.Dir, "stop", id+".request.json"), task.MaxControlRecordSize)
	if err != nil {
		return nil, "", nil, err
	}
	receipt, err := td.ReadSupervisorReceipt()
	if err != nil {
		return nil, "", nil, err
	}
	return r, task.ComputeSHA256(data), receipt, nil
}

func stopEvidenceMatches(request *task.StopRequestRecord, digest string, receipt *task.SupervisorReceipt, root, id, spec, meta, label, requestID, requestSHA string, supervisor *task.SupervisorRef, numeric *int64) bool {
	return root == request.RootID && id == request.TaskID && spec == request.SpecSHA256 && meta == request.MetaSHA256 && label == request.Label && requestID == request.RequestID && requestSHA == digest && supervisor != nil && *supervisor == request.Supervisor && numeric != nil && *numeric == receipt.NumericTaskID
}

func (td *TaskDir) ReadStopReply(id string) (*task.StopReplyRecord, error) {
	request, digest, receipt, err := td.stopEvidence(id)
	if err != nil {
		return nil, err
	}
	var r task.StopReplyRecord
	if err = td.store.readRecord(filepath.Join(td.Dir, "stop", id+".reply.json"), &r); err != nil {
		return nil, err
	}
	if err = task.ValidateStopReplyRecord(&r); err != nil {
		return nil, err
	}
	if !stopEvidenceMatches(request, digest, receipt, r.RootID, r.TaskID, r.SpecSHA256, r.MetaSHA256, r.Label, r.RequestID, r.RequestSHA256, r.Supervisor, r.NumericTaskID) {
		return nil, task.ErrIdentityMismatch
	}
	return &r, nil
}

func (td *TaskDir) ReadStopObservation(id string) (*task.StopObservedRecord, error) {
	request, digest, receipt, err := td.stopEvidence(id)
	if err != nil {
		return nil, err
	}
	var r task.StopObservedRecord
	if err = td.store.readRecord(filepath.Join(td.Dir, "stop", id+".observed.json"), &r); err != nil {
		return nil, err
	}
	if err = task.ValidateStopObservedRecord(&r); err != nil {
		return nil, err
	}
	if !stopEvidenceMatches(request, digest, receipt, r.RootID, r.TaskID, r.SpecSHA256, r.MetaSHA256, r.Label, r.RequestID, r.RequestSHA256, r.Supervisor, r.NumericTaskID) {
		return nil, task.ErrIdentityMismatch
	}
	return &r, nil
}

// ReadStopRecords enumerates the task's rooted stop records within
// MaxStopRecords and MaxStopInspectionBytes. It validates every discovered
// request and optional side record; absent replies or observations remain nil
// so callers can report unknown state without fabricating facts.
func (td *TaskDir) ReadStopRecords() ([]StopRecord, error) {
	if _, _, _, _, _, err := td.loadAndValidatePreparedSet(); err != nil {
		return nil, err
	}
	ids, err := td.discoverStopRecordIDs()
	if err != nil {
		return nil, err
	}
	if err = validateStopRecordIDs(ids); err != nil {
		return nil, err
	}
	return td.readStopRecordRows(ids)
}

func (td *TaskDir) discoverStopRecordIDs() (ids stopRecordIDs, resultErr error) {
	ids = newStopRecordIDs()
	stopDir, err := td.store.openDir(filepath.Join(td.Dir, "stop"))
	if errors.Is(err, os.ErrNotExist) {
		return ids, nil
	}
	if err != nil {
		return stopRecordIDs{}, err
	}
	for {
		names, readErr := stopDir.Readdirnames(1)
		for _, name := range names {
			if resultErr = addStopRecordName(&ids, name); resultErr != nil {
				break
			}
		}
		if resultErr != nil {
			break
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			resultErr = readErr
			break
		}
	}
	resultErr = errors.Join(resultErr, stopDir.Close())
	if resultErr != nil {
		return stopRecordIDs{}, resultErr
	}
	return ids, nil
}

func newStopRecordIDs() stopRecordIDs {
	return stopRecordIDs{
		requests:     make(map[string]struct{}),
		replies:      make(map[string]struct{}),
		observations: make(map[string]struct{}),
	}
}

func addStopRecordName(ids *stopRecordIDs, name string) error {
	// A staged file is an in-progress persistence detail. It is not a durable
	// fact and cannot be interpreted as a missing or present stop record.
	if IsRecognizedStageFile(name) {
		return nil
	}
	kind, id, ok := stopRecordName(name)
	if !ok {
		return fmt.Errorf("%w: unexpected stop entry %q", task.ErrEvidenceFault, name)
	}
	if err := task.ValidateStopRequestID(id); err != nil {
		return fmt.Errorf("%w: invalid stop entry %q", task.ErrEvidenceFault, name)
	}
	if ids.entries >= maxStopRecordEntries {
		return fmt.Errorf("%w: more than %d stop record entries", ErrStopInspectionLimit, maxStopRecordEntries)
	}
	ids.entries++
	switch kind {
	case stopRequestSuffix:
		if len(ids.requests) >= MaxStopRecords {
			return fmt.Errorf("%w: more than %d stop requests", ErrStopInspectionLimit, MaxStopRecords)
		}
		ids.requests[id] = struct{}{}
	case stopReplySuffix:
		ids.replies[id] = struct{}{}
	case stopObservedSuffix:
		ids.observations[id] = struct{}{}
	default:
		return fmt.Errorf("%w: unsupported stop entry %q", task.ErrEvidenceFault, name)
	}
	return nil
}

func validateStopRecordIDs(ids stopRecordIDs) error {
	for id := range ids.replies {
		if _, ok := ids.requests[id]; !ok {
			return fmt.Errorf("%w: stop reply without request %q", task.ErrIdentityMismatch, id)
		}
	}
	for id := range ids.observations {
		if _, ok := ids.requests[id]; !ok {
			return fmt.Errorf("%w: stop observation without request %q", task.ErrIdentityMismatch, id)
		}
	}
	return nil
}

type stopInspectionBudget struct {
	bytes int
}

func (b *stopInspectionBudget) reserve(data []byte) error {
	if len(data) > MaxStopInspectionBytes-b.bytes {
		return fmt.Errorf("%w: encoded stop evidence exceeds %d bytes", ErrStopInspectionLimit, MaxStopInspectionBytes)
	}
	b.bytes += len(data)
	return nil
}

func (td *TaskDir) reserveStopRecord(id, suffix string, budget *stopInspectionBudget) error {
	data, err := td.store.readBytes(filepath.Join(td.Dir, "stop", id+suffix), task.MaxControlRecordSize)
	if err != nil {
		return err
	}
	return budget.reserve(data)
}

func (td *TaskDir) readStopRecordRows(recordIDs stopRecordIDs) ([]StopRecord, error) {
	ids := make([]string, 0, len(recordIDs.requests))
	for id := range recordIDs.requests {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	budget := stopInspectionBudget{}
	rows := make([]StopRecord, 0, len(ids))
	for _, id := range ids {
		row, err := td.readStopRecordRow(id, recordIDs, &budget)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (td *TaskDir) readStopRecordRow(id string, recordIDs stopRecordIDs, budget *stopInspectionBudget) (StopRecord, error) {
	if err := td.reserveStopRecord(id, stopRequestSuffix, budget); err != nil {
		return StopRecord{}, err
	}
	request, err := td.ReadStopRequest(id)
	if err != nil {
		return StopRecord{}, err
	}
	row := StopRecord{Request: request}
	if _, ok := recordIDs.replies[id]; ok {
		if err = td.reserveStopRecord(id, stopReplySuffix, budget); err != nil {
			return StopRecord{}, err
		}
		row.Reply, err = td.ReadStopReply(id)
		if err != nil {
			return StopRecord{}, err
		}
	}
	if _, ok := recordIDs.observations[id]; ok {
		if err = td.reserveStopRecord(id, stopObservedSuffix, budget); err != nil {
			return StopRecord{}, err
		}
		row.Observation, err = td.ReadStopObservation(id)
		if err != nil {
			return StopRecord{}, err
		}
	}
	return row, nil
}

func stopRecordName(name string) (kind, id string, ok bool) {
	for _, suffix := range []string{stopRequestSuffix, stopReplySuffix, stopObservedSuffix} {
		if strings.HasSuffix(name, suffix) {
			id = strings.TrimSuffix(name, suffix)
			if id == "" {
				return "", "", false
			}
			return suffix, id, true
		}
	}
	return "", "", false
}

func (td *TaskDir) RecordStopReply(id string, facts task.StopReplyFacts) (resultErr error) {
	if err := td.store.maintLock.LockSHNonblocking(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, td.store.maintLock.Unlock()) }()
	request, digest, receipt, err := td.stopEvidence(id)
	if err != nil {
		return err
	}
	if facts.NumericTaskID != receipt.NumericTaskID {
		return task.ErrIdentityMismatch
	}
	r := task.StopReplyRecord{SchemaVersion: task.SchemaVersion, RootID: request.RootID, TaskID: request.TaskID, SpecSHA256: request.SpecSHA256, MetaSHA256: request.MetaSHA256, Label: request.Label, Supervisor: &request.Supervisor, RequestID: id, RequestSHA256: digest, NumericTaskID: &facts.NumericTaskID, Action: facts.Action, Acknowledged: facts.Acknowledged, Message: facts.Message, RepliedAt: timestamp()}
	if err = task.ValidateStopReplyRecord(&r); err != nil {
		return err
	}
	return td.recordSame(filepath.Join("stop", id+".reply.json"), r, func() (bool, error) {
		old, e := td.ReadStopReply(id)
		if e != nil {
			return false, e
		}
		r.RepliedAt = old.RepliedAt
		return reflect.DeepEqual(old, &r), nil
	})
}

func (td *TaskDir) RecordStopObservation(id string, facts task.StopObservationFacts) (resultErr error) {
	if !facts.Terminated || facts.State != "ended" {
		return task.ErrIdentityMismatch
	}
	if err := td.store.maintLock.LockSHNonblocking(); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, td.store.maintLock.Unlock()) }()
	request, digest, receipt, err := td.stopEvidence(id)
	if err != nil {
		return err
	}
	if facts.NumericTaskID != receipt.NumericTaskID {
		return task.ErrIdentityMismatch
	}
	r := task.StopObservedRecord{SchemaVersion: task.SchemaVersion, RootID: request.RootID, TaskID: request.TaskID, SpecSHA256: request.SpecSHA256, MetaSHA256: request.MetaSHA256, Label: request.Label, Supervisor: &request.Supervisor, RequestID: id, RequestSHA256: digest, NumericTaskID: &facts.NumericTaskID, Terminated: facts.Terminated, State: facts.State, ObservedAt: timestamp()}
	if err = task.ValidateStopObservedRecord(&r); err != nil {
		return err
	}
	return td.recordSame(filepath.Join("stop", id+".observed.json"), r, func() (bool, error) {
		old, e := td.ReadStopObservation(id)
		if e != nil {
			return false, e
		}
		r.ObservedAt = old.ObservedAt
		return reflect.DeepEqual(old, &r), nil
	})
}
