package inspection

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const journalLockTimeout = 5 * time.Second

// Operation owns one task-bound inspection journal and its immutable request.
// Its control directory is always outside ordinary task records.
type Operation struct {
	control  *taskdir.ControlDir
	request  RequestRecord
	digest   string
	deadline time.Time
}

// LoadOperation opens existing evidence without creating a journal or renewing
// its deadline. Worker startup and ordinary-task replay use this read path.
func LoadOperation(store *taskdir.Store, taskID string) (operation *Operation, resultErr error) {
	return LoadOperationContext(context.Background(), store, taskID)
}

// LoadOperationContext opens existing evidence while honoring the caller's
// deadline for the rooted maintenance acquisition and bounded record read.
// It never creates a journal or renews its persisted deadline.
func LoadOperationContext(ctx context.Context, store *taskdir.Store, taskID string) (operation *Operation, resultErr error) {
	if store == nil {
		return nil, task.ErrEvidenceFault
	}
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	control, err := store.OpenInspectionContext(ctx, taskID, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		if operation == nil {
			resultErr = errors.Join(resultErr, control.Close())
		}
	}()
	var request RequestRecord
	if err = control.Read(requestRecordName, &request); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	deadline, err := validateRequestRecord(request, store.RootID)
	if err != nil || request.TaskID != taskID {
		return nil, task.ErrEvidenceFault
	}
	return &Operation{control: control, request: cloneRequestRecord(request), digest: requestDigest(request), deadline: deadline}, nil
}

// OpenOperation opens or creates the inspection journal for request. Static
// validation occurs before any journal directory is created. A replay reads
// request.json under the operation lock before constructing any new deadline.
func OpenOperation(store *taskdir.Store, request task.TaskRecord, binding Binding, now time.Time) (operation *Operation, resultErr error) {
	if store == nil {
		return nil, errors.New("nil inspection store")
	}
	if err := validateFreshRequest(store, &request); err != nil {
		return nil, err
	}
	if err := ValidateBinding(binding); err != nil {
		return nil, err
	}
	expected, err := expectedRequest(request, binding)
	if err != nil {
		return nil, err
	}
	control, err := store.OpenInspection(request.TaskID, true)
	if err != nil {
		return nil, err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			resultErr = errors.Join(resultErr, control.Close())
		}
	}()

	record, digest, deadline, err := establishRequest(control, store.RootID, expected, now)
	if err != nil {
		return nil, err
	}
	op := &Operation{control: control, request: cloneRequestRecord(record), digest: digest, deadline: deadline}
	closeOnError = false
	return op, nil
}

func expectedRequest(request task.TaskRecord, binding Binding) (RequestRecord, error) {
	taskBytes, err := task.MarshalCanonical(request)
	if err != nil {
		return RequestRecord{}, err
	}
	return RequestRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        request.RootID,
		TaskID:        request.TaskID,
		Task:          cloneTaskRecord(request),
		TaskSHA256:    task.ComputeSHA256(taskBytes),
		Binding:       binding,
		Group:         groupForRoot(request.RootID),
		Label:         labelForIdentity(request.RootID, request.TaskID),
	}, nil
}

func establishRequest(control *taskdir.ControlDir, rootID string, expected RequestRecord, now time.Time) (record RequestRecord, digest string, deadline time.Time, resultErr error) {
	resultErr = withControlTransaction(control, func(tx *taskdir.ControlTransaction) error {
		var existing RequestRecord
		readErr := tx.Read(requestRecordName, &existing)
		if readErr == nil {
			var acceptErr error
			record, digest, deadline, acceptErr = acceptExistingRequest(tx, existing, rootID, expected)
			return acceptErr
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		return createRequest(tx, rootID, expected, now, &record, &digest, &deadline)
	})
	return record, digest, deadline, resultErr
}

func acceptExistingRequest(tx *taskdir.ControlTransaction, existing RequestRecord, rootID string, expected RequestRecord) (RequestRecord, string, time.Time, error) {
	deadline, validateErr := validateRequestRecord(existing, rootID)
	if validateErr != nil {
		return RequestRecord{}, "", time.Time{}, fmt.Errorf("%w: invalid inspection request: %w", task.ErrEvidenceFault, validateErr)
	}
	if !sameImmutableRequest(existing, expected) {
		return RequestRecord{}, "", time.Time{}, fmt.Errorf("%w: inspection request or binding changed", task.ErrEvidenceFault)
	}
	// Put performs the rooted byte-for-byte winner check. Semantic equality is
	// insufficient here: whitespace, field order, or another encoding change
	// would otherwise be accepted while requestDigest silently canonicalized it.
	canonical := expected
	canonical.CreatedAt = existing.CreatedAt
	canonical.Deadline = existing.Deadline
	created, putErr := tx.Put(requestRecordName, canonical)
	if putErr != nil {
		return RequestRecord{}, "", time.Time{}, putErr
	}
	if created {
		return RequestRecord{}, "", time.Time{}, fmt.Errorf("%w: inspection request changed during replay", task.ErrEvidenceFault)
	}
	return existing, requestDigest(existing), deadline, nil
}

func createRequest(tx *taskdir.ControlTransaction, rootID string, expected RequestRecord, now time.Time, record *RequestRecord, digest *string, deadline *time.Time) error {
	if now.IsZero() {
		return ErrInvalidAdmissionTime
	}
	created := now.UTC()
	candidate := expected
	candidate.CreatedAt = created.Format(time.RFC3339Nano)
	candidate.Deadline = created.Add(AdmissionTimeout).Format(time.RFC3339Nano)
	createdRecord, putErr := tx.Put(requestRecordName, candidate)
	if putErr != nil {
		return putErr
	}
	if !createdRecord {
		var winner RequestRecord
		if winnerReadErr := tx.Read(requestRecordName, &winner); winnerReadErr != nil {
			return winnerReadErr
		}
		winnerRecord, winnerDigest, winnerDeadline, acceptErr := acceptExistingRequest(tx, winner, rootID, expected)
		if acceptErr != nil {
			return acceptErr
		}
		*record, *digest, *deadline = winnerRecord, winnerDigest, winnerDeadline
		return nil
	}
	if _, candidateValidateErr := validateRequestRecord(candidate, rootID); candidateValidateErr != nil {
		return fmt.Errorf("%w: constructed inspection request invalid: %w", task.ErrInvariantFault, candidateValidateErr)
	}
	*record, *digest, *deadline = candidate, requestDigest(candidate), created.Add(AdmissionTimeout)
	return nil
}

func validateFreshRequest(store *taskdir.Store, request *task.TaskRecord) error {
	if request == nil {
		return errors.New("nil inspection task request")
	}
	if request.RootID != store.RootID {
		return task.ErrIdentityMismatch
	}
	normalized := cloneTaskRecord(*request)
	if err := task.NormalizeRequestedConfig(&normalized); err != nil {
		return err
	}
	if !reflect.DeepEqual(normalized, *request) {
		return task.ErrRequestConflict
	}
	return task.ValidateTaskRecord(request)
}

func sameImmutableRequest(left, right RequestRecord) bool {
	return left.SchemaVersion == right.SchemaVersion &&
		left.RootID == right.RootID &&
		left.TaskID == right.TaskID &&
		left.TaskSHA256 == right.TaskSHA256 &&
		reflect.DeepEqual(left.Task, right.Task) &&
		reflect.DeepEqual(left.Binding, right.Binding) &&
		left.Group == right.Group &&
		left.Label == right.Label
}

func requestDigest(record RequestRecord) string {
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return ""
	}
	return task.ComputeSHA256(data)
}

func withControlTransaction(control *taskdir.ControlDir, fn func(*taskdir.ControlTransaction) error) error {
	return withControlTransactionContext(context.Background(), control, fn)
}

// withControlTransactionContext bounds journal locking by both the caller's
// lifetime and the fixed local lock ceiling. The explicit precheck matters for
// a free/refcounted lock: WithLock may otherwise acquire it despite an already
// canceled caller and publish evidence after its deadline.
func withControlTransactionContext(ctx context.Context, control *taskdir.ControlDir, fn func(*taskdir.ControlTransaction) error) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	bounded, cancel := context.WithTimeout(ctx, journalLockTimeout)
	defer cancel()
	if err := bounded.Err(); err != nil {
		return err
	}
	return control.WithLock(bounded, fn)
}

// Request returns a deep copy of the immutable request record.
func (o *Operation) Request() RequestRecord {
	if o == nil {
		return RequestRecord{}
	}
	return cloneRequestRecord(o.request)
}

// Digest returns the canonical digest of request.json.
func (o *Operation) Digest() string {
	if o == nil {
		return ""
	}
	return o.digest
}

// Deadline returns the immutable admission deadline.
func (o *Operation) Deadline() time.Time {
	if o == nil {
		return time.Time{}
	}
	return o.deadline
}

// Expired reports whether now is at or after the persisted deadline.
func (o *Operation) Expired(now time.Time) bool {
	if o == nil || now.IsZero() {
		return true
	}
	return !now.Before(o.deadline)
}

// Close releases the operation journal handle.
func (o *Operation) Close() error {
	if o == nil || o.control == nil {
		return nil
	}
	return o.control.Close()
}

// ClaimSubmission creates the single-use supervisor admission permit. An
// existing guard never yields authority, even when its bytes match.
func (o *Operation) ClaimSubmission(now time.Time) (*SubmissionPermit, error) {
	return o.ClaimSubmissionContext(context.Background(), now)
}

// ClaimSubmissionContext creates the single-use submission permit while
// honoring the caller's lifetime for the durable claim transaction.
func (o *Operation) ClaimSubmissionContext(ctx context.Context, now time.Time) (*SubmissionPermit, error) {
	if err := o.checkClaimTime(now); err != nil {
		return nil, err
	}
	created, err := o.claimGuardContext(ctx, submissionRecordName, now, func(createdAt string) any {
		return SubmissionRecord{SchemaVersion: task.SchemaVersion, RequestSHA256: o.Digest(), CreatedAt: createdAt}
	})
	if err != nil {
		return nil, err
	}
	if !created {
		return nil, task.ErrAlreadySubmitted
	}
	return &SubmissionPermit{
		state:      newOneShot(),
		rootID:     o.request.RootID,
		taskID:     o.request.TaskID,
		supervisor: o.request.Binding.Supervisor,
	}, nil
}

// ClaimStart creates the single-use worker start permit. An existing guard
// never yields authority, even when its bytes match.
func (o *Operation) ClaimStart(now time.Time) (*StartPermit, error) {
	if err := o.checkClaimTime(now); err != nil {
		return nil, err
	}
	created, err := o.claimGuard(startRecordName, now, func(createdAt string) any {
		return StartRecord{SchemaVersion: task.SchemaVersion, RequestSHA256: o.Digest(), CreatedAt: createdAt}
	})
	if err != nil {
		return nil, err
	}
	if !created {
		return nil, task.ErrAlreadyStarted
	}
	return &StartPermit{state: newOneShot()}, nil
}

func canonicalNow(now time.Time) string {
	if now.IsZero() {
		return ""
	}
	return now.UTC().Format(time.RFC3339Nano)
}

func (o *Operation) checkClaimTime(now time.Time) error {
	if o == nil || o.control == nil {
		return task.ErrInvalidPermit
	}
	if now.IsZero() {
		return ErrInvalidAdmissionTime
	}
	created, err := canonicalTimestamp("inspection creation", o.request.CreatedAt)
	if err != nil || now.Before(created) {
		return ErrInvalidAdmissionTime
	}
	if !now.Before(o.deadline) {
		return ErrAdmissionExpired
	}
	return nil
}

func (o *Operation) claimGuard(name string, now time.Time, makeRecord func(string) any) (bool, error) {
	return o.claimGuardContext(context.Background(), name, now, makeRecord)
}

func (o *Operation) claimGuardContext(ctx context.Context, name string, now time.Time, makeRecord func(string) any) (bool, error) {
	if o == nil || o.control == nil {
		return false, os.ErrClosed
	}
	if makeRecord == nil {
		return false, task.ErrInvariantFault
	}
	createdAt := canonicalNow(now)
	createdFloor, floorErr := canonicalTimestamp("inspection creation", o.request.CreatedAt)
	if floorErr != nil {
		return false, task.ErrEvidenceFault
	}
	if !createdFloor.Before(o.deadline) {
		return false, task.ErrEvidenceFault
	}
	var created bool
	err := withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		exists, readErr := readExistingClaim(tx, name, o.digest, createdFloor, o.deadline)
		if readErr != nil {
			return readErr
		}
		if exists {
			return nil
		}
		record := makeRecord(createdAt)
		if validateErr := validateOperationClaim(record, o.digest, createdFloor, o.deadline); validateErr != nil {
			return validateErr
		}
		var putErr error
		created, putErr = tx.Put(name, record)
		return putErr
	})
	return created, err
}

func readExistingClaim(tx *taskdir.ControlTransaction, name, requestSHA string, createdFloor, deadline time.Time) (bool, error) {
	var record any
	switch name {
	case submissionRecordName:
		record = new(SubmissionRecord)
	case startRecordName:
		record = new(StartRecord)
	default:
		return false, task.ErrInvariantFault
	}
	readErr := tx.Read(name, record)
	if errors.Is(readErr, os.ErrNotExist) {
		return false, nil
	}
	if readErr != nil {
		return false, claimReadError(name, readErr)
	}
	if validateErr := validateOperationClaim(record, requestSHA, createdFloor, deadline); validateErr != nil {
		return false, fmt.Errorf("%w: invalid persisted %s: %w", task.ErrEvidenceFault, name, validateErr)
	}
	return true, nil
}

func claimReadError(name string, readErr error) error {
	var pathErr *os.PathError
	if errors.As(readErr, &pathErr) || errors.Is(readErr, os.ErrClosed) {
		return readErr
	}
	return fmt.Errorf("%w: invalid persisted %s: %w", task.ErrEvidenceFault, name, readErr)
}

func validateOperationClaim(record any, requestSHA string, createdFloor, deadline time.Time) error {
	digest, createdAt, claimErr := claimFields(record)
	if claimErr != nil {
		return claimErr
	}
	if digest != requestSHA {
		return task.ErrEvidenceFault
	}
	created, err := canonicalTimestamp("inspection claim created_at", createdAt)
	if err != nil || created.Before(createdFloor) || !created.Before(deadline) {
		return task.ErrEvidenceFault
	}
	return nil
}

func claimFields(record any) (digest, createdAt string, resultErr error) {
	switch value := record.(type) {
	case SubmissionRecord:
		if err := validateClaim(value); err != nil {
			return "", "", err
		}
		return value.RequestSHA256, value.CreatedAt, nil
	case *SubmissionRecord:
		if value == nil {
			return "", "", task.ErrEvidenceFault
		}
		if err := validateClaim(*value); err != nil {
			return "", "", err
		}
		return value.RequestSHA256, value.CreatedAt, nil
	case StartRecord:
		if err := validateClaim(value); err != nil {
			return "", "", err
		}
		return value.RequestSHA256, value.CreatedAt, nil
	case *StartRecord:
		if value == nil {
			return "", "", task.ErrEvidenceFault
		}
		if err := validateClaim(*value); err != nil {
			return "", "", err
		}
		return value.RequestSHA256, value.CreatedAt, nil
	default:
		return "", "", task.ErrInvariantFault
	}
}

func validateClaim(record any) error {
	switch value := record.(type) {
	case SubmissionRecord:
		return validateClaimRecord(value.SchemaVersion, value.RequestSHA256, value.CreatedAt)
	case StartRecord:
		return validateClaimRecord(value.SchemaVersion, value.RequestSHA256, value.CreatedAt)
	default:
		return task.ErrInvariantFault
	}
}

type oneShot struct {
	mu       sync.Mutex
	consumed bool
}

func newOneShot() *oneShot { return &oneShot{} }

func (s *oneShot) consume() error {
	if s == nil {
		return task.ErrInvalidPermit
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.consumed {
		return task.ErrPermitAlreadyUsed
	}
	s.consumed = true
	return nil
}

// SubmissionPermit authorizes one already-recorded submission intent. The
// identity and supervisor binding are copied from the saved request at claim
// time and are never supplied by the pueue caller.
type SubmissionPermit struct {
	state      *oneShot
	rootID     string
	taskID     string
	supervisor task.SupervisorRef
}

// ConsumeInspectionSubmission marks the submission permit consumed before an
// external supervisor mutation. Its name is action-specific so this permit
// cannot satisfy pueue's group or stop interface.
func (p *SubmissionPermit) ConsumeInspectionSubmission() error {
	if p == nil {
		return task.ErrInvalidPermit
	}
	return p.state.consume()
}

// InspectionSubmissionRootID returns the root identity saved in the request.
func (p *SubmissionPermit) InspectionSubmissionRootID() string {
	if p == nil {
		return ""
	}
	return p.rootID
}

// InspectionSubmissionTaskID returns the task identity saved in the request.
func (p *SubmissionPermit) InspectionSubmissionTaskID() string {
	if p == nil {
		return ""
	}
	return p.taskID
}

// InspectionSubmissionSupervisor returns the supervisor binding saved in the
// request. The value is returned by copy.
func (p *SubmissionPermit) InspectionSubmissionSupervisor() task.SupervisorRef {
	if p == nil {
		return task.SupervisorRef{}
	}
	return p.supervisor
}

// Consume retains the journal-level one-shot API for callers that consume a
// submission permit directly. The pueue boundary uses the action-specific
// method above.
func (p *SubmissionPermit) Consume() error {
	return p.ConsumeInspectionSubmission()
}

var _ pueue.InspectionSubmissionPermit = (*SubmissionPermit)(nil)

// StartPermit authorizes one already-recorded worker start intent.
type StartPermit struct{ state *oneShot }

// Consume marks the start permit consumed before any external action.
func (p *StartPermit) Consume() error {
	if p == nil {
		return task.ErrInvalidPermit
	}
	return p.state.consume()
}
