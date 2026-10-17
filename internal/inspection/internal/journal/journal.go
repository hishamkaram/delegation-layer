package journal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection/contract"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Operation owns one task-bound inspection journal and its immutable request.
// Its control directory is always outside ordinary task records.
type Operation struct {
	control  *taskdir.ControlDir
	request  contract.RequestRecord
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
	var request contract.RequestRecord
	var digest string
	var deadline time.Time
	err = withControlTransactionContext(ctx, control, func(tx *taskdir.ControlTransaction) error {
		if readErr := tx.Read(requestRecordName, &request); readErr != nil {
			return readErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		validatedDeadline, validationErr := contract.RequestDeadline(request, store.RootID)
		if validationErr != nil || request.IdentityID() != taskID {
			return task.ErrEvidenceFault
		}
		if groupErr := validateOperationGroup(store, request); groupErr != nil {
			return groupErr
		}
		if canonicalErr := verifyCanonicalRequest(tx, request); canonicalErr != nil {
			return canonicalErr
		}
		digest = contract.RequestDigest(request)
		deadline = validatedDeadline
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Operation{control: control, request: contract.CloneRequestRecord(request), digest: digest, deadline: deadline}, nil
}

// OpenOperation opens or creates the inspection journal for request. Static
// validation occurs before any journal directory is created. A replay reads
// request.json under the operation lock before constructing any new deadline.
func OpenOperation(store *taskdir.Store, request task.TaskRecord, binding contract.Binding, now time.Time) (*Operation, error) {
	return OpenOperationContext(context.Background(), store, request, binding, now)
}

// OpenOperationContext opens or creates an inspection journal while preserving
// the caller's finite setup lifetime.
func OpenOperationContext(ctx context.Context, store *taskdir.Store, request task.TaskRecord, binding contract.Binding, now time.Time) (*Operation, error) {
	return openOperationContext(ctx, store, request, binding, now, contract.GroupForRoot(request.RootID), "")
}

// OpenOperationWithIdentityContext gives an admission attempt its own journal
// without changing the normalized logical task or an earlier attempt.
func OpenOperationWithIdentityContext(ctx context.Context, store *taskdir.Store, request task.TaskRecord, binding contract.Binding, operationID string, now time.Time) (*Operation, error) {
	if err := task.ValidateTaskID(operationID); err != nil {
		return nil, err
	}
	return openOperationContext(ctx, store, request, binding, now, contract.GroupForRoot(request.RootID), operationID)
}

// OpenConcurrentOperation opens a runtime capability inspection in the private
// supervisor's ordinary default group. Saved requests keep their group and
// deadline; this entrypoint never migrates a historical inspection journal.
func OpenConcurrentOperation(store *taskdir.Store, request task.TaskRecord, binding contract.Binding, now time.Time) (*Operation, error) {
	return OpenConcurrentOperationContext(context.Background(), store, request, binding, now)
}

// OpenConcurrentOperationContext opens or creates a concurrent inspection
// journal while preserving the caller's finite setup lifetime.
func OpenConcurrentOperationContext(ctx context.Context, store *taskdir.Store, request task.TaskRecord, binding contract.Binding, now time.Time) (*Operation, error) {
	return openOperationContext(ctx, store, request, binding, now, "default", "")
}

func OpenConcurrentOperationWithIdentityContext(ctx context.Context, store *taskdir.Store, request task.TaskRecord, binding contract.Binding, operationID string, now time.Time) (*Operation, error) {
	if err := task.ValidateTaskID(operationID); err != nil {
		return nil, err
	}
	return openOperationContext(ctx, store, request, binding, now, "default", operationID)
}

func openOperationContext(ctx context.Context, store *taskdir.Store, request task.TaskRecord, binding contract.Binding, now time.Time, group, operationID string) (operation *Operation, resultErr error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, errors.New("nil inspection store")
	}
	if err := validateFreshRequest(store, &request); err != nil {
		return nil, err
	}
	if err := contract.ValidateBinding(binding); err != nil {
		return nil, err
	}
	expected, err := expectedRequest(request, binding)
	if err != nil {
		return nil, err
	}
	expected.Group = group
	expected.OperationID = operationID
	expected.Label = contract.LabelForIdentity(request.RootID, expected.IdentityID())
	if err = validateOperationGroup(store, expected); err != nil {
		return nil, err
	}
	control, err := store.OpenInspectionContext(ctx, expected.IdentityID(), true)
	if err != nil {
		return nil, err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			resultErr = errors.Join(resultErr, control.Close())
		}
	}()

	record, digest, deadline, err := establishRequestContext(ctx, control, store.RootID, expected, now)
	if err != nil {
		return nil, err
	}
	op := &Operation{control: control, request: contract.CloneRequestRecord(record), digest: digest, deadline: deadline}
	closeOnError = false
	return op, nil
}

func validateOperationGroup(store *taskdir.Store, record contract.RequestRecord) error {
	if err := contract.ValidateRequestGroup(record); err != nil {
		return err
	}
	if record.Group == "default" && !pueue.IsPrivateConfig(store.Root, record.Binding.Supervisor.ConfigPath) {
		return fmt.Errorf("%w: default inspection group requires the store's private supervisor config", task.ErrEvidenceFault)
	}
	return nil
}

func expectedRequest(request task.TaskRecord, binding contract.Binding) (contract.RequestRecord, error) {
	taskBytes, err := task.MarshalCanonical(request)
	if err != nil {
		return contract.RequestRecord{}, err
	}
	return contract.RequestRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        request.RootID,
		TaskID:        request.TaskID,
		Task:          contract.CloneTaskRecord(request),
		TaskSHA256:    task.ComputeSHA256(taskBytes),
		Binding:       binding,
		Group:         contract.GroupForRoot(request.RootID),
		Label:         contract.LabelForIdentity(request.RootID, request.TaskID),
	}, nil
}

func establishRequestContext(ctx context.Context, control *taskdir.ControlDir, rootID string, expected contract.RequestRecord, now time.Time) (record contract.RequestRecord, digest string, deadline time.Time, resultErr error) {
	resultErr = withControlTransactionContext(ctx, control, func(tx *taskdir.ControlTransaction) error {
		var existing contract.RequestRecord
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

func acceptExistingRequest(tx *taskdir.ControlTransaction, existing contract.RequestRecord, rootID string, expected contract.RequestRecord) (contract.RequestRecord, string, time.Time, error) {
	deadline, validateErr := contract.RequestDeadline(existing, rootID)
	if validateErr != nil {
		return contract.RequestRecord{}, "", time.Time{}, fmt.Errorf("%w: invalid inspection request: %w", task.ErrEvidenceFault, validateErr)
	}
	if !sameImmutableRequest(existing, expected) {
		return contract.RequestRecord{}, "", time.Time{}, fmt.Errorf("%w: inspection request or binding changed", task.ErrEvidenceFault)
	}
	// Verify performs the rooted byte-for-byte winner check. Semantic equality is
	// insufficient here: whitespace, field order, or another encoding change
	// would otherwise be accepted while requestDigest silently canonicalized it.
	canonical := expected
	canonical.CreatedAt = existing.CreatedAt
	canonical.Deadline = existing.Deadline
	if err := verifyCanonicalRequest(tx, canonical); err != nil {
		return contract.RequestRecord{}, "", time.Time{}, err
	}
	return existing, contract.RequestDigest(existing), deadline, nil
}

func verifyCanonicalRequest(tx *taskdir.ControlTransaction, request contract.RequestRecord) error {
	return tx.Verify(requestRecordName, request)
}

func createRequest(tx *taskdir.ControlTransaction, rootID string, expected contract.RequestRecord, now time.Time, record *contract.RequestRecord, digest *string, deadline *time.Time) error {
	if now.IsZero() {
		return contract.ErrInvalidAdmissionTime
	}
	created := now.UTC()
	candidate := expected
	candidate.CreatedAt = created.Format(time.RFC3339Nano)
	candidate.Deadline = created.Add(contract.InspectionTimeout(candidate.Binding)).Format(time.RFC3339Nano)
	createdRecord, putErr := tx.Put(requestRecordName, candidate)
	if putErr != nil {
		return putErr
	}
	if !createdRecord {
		var winner contract.RequestRecord
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
	if _, candidateValidateErr := contract.RequestDeadline(candidate, rootID); candidateValidateErr != nil {
		return fmt.Errorf("%w: constructed inspection request invalid: %w", task.ErrInvariantFault, candidateValidateErr)
	}
	*record, *digest, *deadline = candidate, contract.RequestDigest(candidate), created.Add(contract.InspectionTimeout(candidate.Binding))
	return nil
}

func validateFreshRequest(store *taskdir.Store, request *task.TaskRecord) error {
	if request == nil {
		return errors.New("nil inspection task request")
	}
	if request.RootID != store.RootID {
		return task.ErrIdentityMismatch
	}
	normalized := contract.CloneTaskRecord(*request)
	if err := task.NormalizeRequestedConfig(&normalized); err != nil {
		return err
	}
	if !reflect.DeepEqual(normalized, *request) {
		return task.ErrRequestConflict
	}
	return task.ValidateTaskRecord(request)
}

func sameImmutableRequest(left, right contract.RequestRecord) bool {
	return left.SchemaVersion == right.SchemaVersion &&
		left.RootID == right.RootID &&
		left.TaskID == right.TaskID &&
		left.OperationID == right.OperationID &&
		left.TaskSHA256 == right.TaskSHA256 &&
		reflect.DeepEqual(left.Task, right.Task) &&
		reflect.DeepEqual(left.Binding, right.Binding) &&
		left.Group == right.Group &&
		left.Label == right.Label
}

func withControlTransaction(control *taskdir.ControlDir, fn func(*taskdir.ControlTransaction) error) error {
	return withControlTransactionContext(context.Background(), control, fn)
}

// withControlTransactionContext preserves the caller's authoritative lifetime;
// taskdir supplies the finite fallback when no deadline exists. The explicit
// precheck matters for a free/refcounted lock: WithLock may otherwise acquire it
// despite an already canceled caller and publish evidence after its deadline.
func withControlTransactionContext(ctx context.Context, control *taskdir.ControlDir, fn func(*taskdir.ControlTransaction) error) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return control.WithLock(ctx, fn)
}

// Request returns a deep copy of the immutable request record.
func (o *Operation) Request() contract.RequestRecord {
	if o == nil {
		return contract.RequestRecord{}
	}
	return contract.CloneRequestRecord(o.request)
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
		return contract.SubmissionRecord{SchemaVersion: task.SchemaVersion, RequestSHA256: o.Digest(), CreatedAt: createdAt}
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
		taskID:     o.request.IdentityID(),
		queueGroup: o.request.Group,
		supervisor: o.request.Binding.Supervisor,
	}, nil
}

// ClaimStart creates the single-use worker start permit. An existing guard
// never yields authority, even when its bytes match.
func (o *Operation) ClaimStart(now time.Time) (*StartPermit, error) {
	return o.ClaimStartContext(context.Background(), now)
}

// ClaimStartContext creates the single-use worker start permit while honoring
// the caller's lifetime for the durable claim transaction.
func (o *Operation) ClaimStartContext(ctx context.Context, now time.Time) (*StartPermit, error) {
	if err := o.checkClaimTime(now); err != nil {
		return nil, err
	}
	created, err := o.claimGuardContext(ctx, startRecordName, now, func(createdAt string) any {
		return contract.StartRecord{SchemaVersion: task.SchemaVersion, RequestSHA256: o.Digest(), CreatedAt: createdAt}
	})
	if err != nil {
		return nil, err
	}
	if !created {
		return nil, task.ErrAlreadyStarted
	}
	return &StartPermit{state: newOneShot()}, nil
}

func (o *Operation) checkClaimTime(now time.Time) error {
	if o == nil || o.control == nil {
		return task.ErrInvalidPermit
	}
	if now.IsZero() {
		return contract.ErrInvalidAdmissionTime
	}
	created, err := contract.CanonicalTimestamp("inspection creation", o.request.CreatedAt)
	if err != nil || now.Before(created) {
		return contract.ErrInvalidAdmissionTime
	}
	if !now.Before(o.deadline) {
		return contract.ErrAdmissionExpired
	}
	return nil
}

func (o *Operation) claimGuardContext(ctx context.Context, name string, now time.Time, makeRecord func(string) any) (bool, error) {
	if o == nil || o.control == nil {
		return false, os.ErrClosed
	}
	if makeRecord == nil {
		return false, task.ErrInvariantFault
	}
	createdAt := contract.CanonicalNow(now)
	createdFloor, floorErr := contract.CanonicalTimestamp("inspection creation", o.request.CreatedAt)
	if floorErr != nil {
		return false, task.ErrEvidenceFault
	}
	if !createdFloor.Before(o.deadline) {
		return false, task.ErrEvidenceFault
	}
	// The persisted deadline and caller-supplied claim timestamp share the
	// caller's admission clock. Translate their remaining interval onto this
	// transaction's wall clock so lock wait consumes the same authority window.
	claimStarted := time.Now()
	claimDeadline := claimStarted.Add(o.deadline.Sub(now))
	claimCtx, cancel, contextErr := claimTransactionContext(ctx, claimDeadline)
	if contextErr != nil {
		return false, contextErr
	}
	defer cancel()
	records, lifetimeErr := o.controlLifetime(true)
	if lifetimeErr != nil {
		return false, lifetimeErr
	}
	var created bool
	err := records.WithLifetimeLaunchPermissionOrReplayContext(claimCtx, func(tx *taskdir.ControlTransaction) error {
		exists, readErr := readExistingClaim(tx, name, o.digest, createdFloor, o.deadline)
		if readErr != nil {
			return readErr
		}
		if err := claimContextErr(claimCtx); err != nil {
			return err
		}
		if exists {
			return nil
		}
		return os.ErrNotExist
	}, func(tx *taskdir.ControlTransaction) error {
		record := makeRecord(createdAt)
		if validateErr := validateOperationClaim(record, o.digest, createdFloor, o.deadline); validateErr != nil {
			return validateErr
		}
		if !time.Now().Before(claimDeadline) {
			return contract.ErrAdmissionExpired
		}
		if err := claimContextErr(claimCtx); err != nil {
			return err
		}
		var putErr error
		created, putErr = tx.Put(name, record)
		return putErr
	})
	return created, err
}

func claimTransactionContext(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc, error) {
	if ctx == nil {
		return nil, nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if !time.Now().Before(deadline) {
		return nil, nil, contract.ErrAdmissionExpired
	}
	claimCtx, cancel := context.WithDeadline(ctx, deadline)
	return claimCtx, cancel, nil
}

func claimContextErr(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

func readExistingClaim(tx *taskdir.ControlTransaction, name, requestSHA string, createdFloor, deadline time.Time) (bool, error) {
	var record any
	switch name {
	case submissionRecordName:
		record = new(contract.SubmissionRecord)
	case startRecordName:
		record = new(contract.StartRecord)
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
	created, err := contract.CanonicalTimestamp("inspection claim created_at", createdAt)
	if err != nil || created.Before(createdFloor) || !created.Before(deadline) {
		return task.ErrEvidenceFault
	}
	return nil
}

func claimFields(record any) (digest, createdAt string, resultErr error) {
	switch value := record.(type) {
	case contract.SubmissionRecord:
		if err := validateClaim(value); err != nil {
			return "", "", err
		}
		return value.RequestSHA256, value.CreatedAt, nil
	case *contract.SubmissionRecord:
		if value == nil {
			return "", "", task.ErrEvidenceFault
		}
		if err := validateClaim(*value); err != nil {
			return "", "", err
		}
		return value.RequestSHA256, value.CreatedAt, nil
	case contract.StartRecord:
		if err := validateClaim(value); err != nil {
			return "", "", err
		}
		return value.RequestSHA256, value.CreatedAt, nil
	case *contract.StartRecord:
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
	case contract.SubmissionRecord:
		return contract.ValidateClaimRecord(value.SchemaVersion, value.RequestSHA256, value.CreatedAt)
	case contract.StartRecord:
		return contract.ValidateClaimRecord(value.SchemaVersion, value.RequestSHA256, value.CreatedAt)
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
	queueGroup string
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

// InspectionQueueGroup returns the queue group saved in the request.
func (p *SubmissionPermit) InspectionQueueGroup() string {
	if p == nil {
		return ""
	}
	return p.queueGroup
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
