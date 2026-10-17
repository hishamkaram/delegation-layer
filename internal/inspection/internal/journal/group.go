package journal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection/contract"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	groupRequestRecordName     = "request.json"
	groupCreationRecordName    = "creation.json"
	groupObservationRecordName = "observation.json"
	groupMigrationRecordName   = "migration.json"
	groupMigrationDoneName     = "migration-complete.json"
	groupMigrationRecordPrefix = "migration-"
)

// GroupJournal owns the root-scoped inspection-group control directory.
// Records are outside ordinary task directories and contain no task payloads.
type GroupJournal struct {
	control    *taskdir.ControlDir
	request    contract.GroupRequestRecord
	requestSHA string
}

// GroupPermit authorizes exactly one already-recorded group creation attempt.
// It cannot authorize a task submission, worker start, or any other command.
type GroupPermit struct {
	state      *oneShot
	rootID     string
	supervisor task.SupervisorRef
}

// ConsumeInspectionGroup marks the group-creation permit consumed before an
// external supervisor mutation. Its name is action-specific so this permit
// cannot satisfy pueue's submission or stop interface.
func (p *GroupPermit) ConsumeInspectionGroup() error {
	if p == nil {
		return task.ErrInvalidPermit
	}
	return p.state.consume()
}

// InspectionGroupRootID returns the root identity saved in the group request.
func (p *GroupPermit) InspectionGroupRootID() string {
	if p == nil {
		return ""
	}
	return p.rootID
}

// InspectionGroupSupervisor returns the supervisor binding saved in the group
// request. The value is returned by copy.
func (p *GroupPermit) InspectionGroupSupervisor() task.SupervisorRef {
	if p == nil {
		return task.SupervisorRef{}
	}
	return p.supervisor
}

// Consume retains the journal-level one-shot API for callers that consume a
// group permit directly. The pueue boundary uses ConsumeInspectionGroup.
func (p *GroupPermit) Consume() error {
	return p.ConsumeInspectionGroup()
}

// OpenGroup opens the root-scoped inspection group journal after validating
// the complete fresh supervisor binding. The journal retains the original
// installation provenance while replay is bound to the logical supervisor
// config and endpoint. Static validation happens before the journal directory
// is opened or created.
func OpenGroup(store *taskdir.Store, binding task.SupervisorRef) (journal *GroupJournal, resultErr error) {
	return OpenGroupContext(context.Background(), store, binding)
}

// OpenGroupContext opens the root-scoped inspection group while preserving the
// caller's bounded admission context.
func OpenGroupContext(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef) (*GroupJournal, error) {
	if ctx == nil {
		return nil, errors.New("nil inspection context")
	}
	return openGroup(ctx, store, binding)
}

// OpenGroupGeneration opens the exact supervisor generation recorded by the
// binding without consulting or changing the root migration head. Historical
// continuations use this path after a successor generation has been published;
// they must keep using their recorded journal generation.
func OpenGroupGeneration(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef) (*GroupJournal, error) {
	if ctx == nil {
		return nil, errors.New("nil inspection context")
	}
	expected, err := expectedGroupRequest(store, binding)
	if err != nil {
		return nil, err
	}
	return openGroupGeneration(ctx, store, binding, expected)
}

// OpenGroupHistorical reopens the exact journal generation recorded by a
// continuation. The initial generation is the root journal, including after
// later migrations; only a published successor uses a generation directory.
func OpenGroupHistorical(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef) (*GroupJournal, error) {
	if ctx == nil {
		return nil, errors.New("nil inspection context")
	}
	root, err := ReadGroupRequest(ctx, store)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: inspection group root journal is missing", task.ErrEvidenceFault)
	}
	if err != nil {
		return nil, err
	}
	if task.SameSupervisorIdentity(root.Supervisor, binding) {
		return OpenGroupContext(ctx, store, binding)
	}
	expected, err := expectedGroupRequest(store, binding)
	if err != nil {
		return nil, err
	}
	published, err := groupGenerationPublished(ctx, store, root, expected)
	if err != nil {
		return nil, err
	}
	if !published {
		return nil, fmt.Errorf("%w: historical inspection supervisor is not a published generation", task.ErrEvidenceFault)
	}
	return openExistingGroupGeneration(ctx, store, binding, expected)
}

func groupGenerationPublished(ctx context.Context, store *taskdir.Store, current, expected contract.GroupRequestRecord) (bool, error) {
	seen := map[string]struct{}{groupRequestDigest(current): {}}
	for {
		if contract.SameGroupRequest(current, expected) {
			return true, nil
		}
		record, found, err := readMigrationRecord(ctx, store, current, groupMigrationDoneName)
		if err != nil {
			return false, err
		}
		if !found {
			return false, nil
		}
		if err := validateMigrationRecord(record, store.RootID, current); err != nil {
			return false, fmt.Errorf("%w: invalid group migration chain: %w", task.ErrEvidenceFault, err)
		}
		digest := groupRequestDigest(record.Successor)
		if _, exists := seen[digest]; exists {
			return false, fmt.Errorf("%w: cyclic group migration chain", task.ErrEvidenceFault)
		}
		seen[digest] = struct{}{}
		current = record.Successor
	}
}

func openExistingGroupGeneration(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, expected contract.GroupRequestRecord) (*GroupJournal, error) {
	generation := contract.GroupGeneration(binding)
	// A completed migration marker is the durable publication point for this
	// generation. Materialize its directory on replay if a crash happened after
	// the marker was synced but before the directory was created.
	control, err := store.OpenInspectionGroupGenerationContext(ctx, generation, true)
	if err != nil {
		return nil, err
	}
	request, readErr := readGroupRequest(ctx, control)
	if errors.Is(readErr, os.ErrNotExist) {
		// The published marker is the authority for this successor. If the
		// process crashed before its request record was materialized, establish
		// the validated expected record in the newly recreated directory.
		return openGroupControl(ctx, control, store.RootID, binding, expected)
	}
	if readErr != nil {
		return nil, errors.Join(readErr, control.Close())
	}
	if !contract.SameGroupRequest(request, expected) {
		return nil, errors.Join(fmt.Errorf("%w: retained inspection generation does not match binding", task.ErrEvidenceFault), control.Close())
	}
	requestBytes, marshalErr := task.MarshalCanonical(request)
	if marshalErr != nil {
		return nil, errors.Join(marshalErr, control.Close())
	}
	return &GroupJournal{control: control, request: request, requestSHA: task.ComputeSHA256(requestBytes)}, nil
}

// ReadGroupRequest returns the immutable legacy group binding without creating
// a journal or contacting the supervisor.
func ReadGroupRequest(ctx context.Context, store *taskdir.Store) (record contract.GroupRequestRecord, resultErr error) {
	if store == nil {
		return contract.GroupRequestRecord{}, errors.New("nil inspection store")
	}
	control, err := store.OpenInspectionGroupContext(ctx, false)
	if err != nil {
		return contract.GroupRequestRecord{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, control.Close()) }()
	record, err = readGroupRequest(ctx, control)
	if errors.Is(err, os.ErrNotExist) {
		return contract.GroupRequestRecord{}, fmt.Errorf("%w: inspection group request is missing", task.ErrEvidenceFault)
	}
	return record, err
}

func openGroup(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef) (journal *GroupJournal, resultErr error) {
	expected, err := expectedGroupRequest(store, binding)
	if err != nil {
		return nil, err
	}
	control, err := store.OpenInspectionGroupContext(ctx, true)
	if err != nil {
		return nil, err
	}
	return openGroupControl(ctx, control, store.RootID, binding, expected)
}

func openGroupGeneration(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, expected contract.GroupRequestRecord) (*GroupJournal, error) {
	generation := contract.GroupGeneration(binding)
	control, err := store.OpenInspectionGroupGenerationContext(ctx, generation, true)
	if err != nil {
		return nil, err
	}
	return openGroupControl(ctx, control, store.RootID, binding, expected)
}

func expectedGroupRequest(store *taskdir.Store, binding task.SupervisorRef) (contract.GroupRequestRecord, error) {
	if store == nil {
		return contract.GroupRequestRecord{}, errors.New("nil inspection store")
	}
	if rootErr := task.ValidateRootID(store.RootID); rootErr != nil {
		return contract.GroupRequestRecord{}, rootErr
	}
	if bindingErr := task.ValidateFreshSupervisorRef(binding); bindingErr != nil {
		return contract.GroupRequestRecord{}, bindingErr
	}
	expected := contract.GroupRequestRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        store.RootID,
		Supervisor:    binding,
		Name:          contract.GroupForRoot(store.RootID),
		ParallelTasks: 1,
	}
	if requestErr := contract.ValidateGroupRequest(expected, store.RootID, binding); requestErr != nil {
		return contract.GroupRequestRecord{}, fmt.Errorf("%w: constructed group request: %w", task.ErrInvariantFault, requestErr)
	}
	return expected, nil
}

func openGroupControl(ctx context.Context, control *taskdir.ControlDir, rootID string, binding task.SupervisorRef, expected contract.GroupRequestRecord) (journal *GroupJournal, resultErr error) {
	if control == nil {
		return nil, errors.New("nil inspection group control")
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			resultErr = errors.Join(resultErr, control.Close())
		}
	}()

	winner, establishErr := establishGroupRequest(ctx, control, rootID, binding, expected)
	if establishErr != nil {
		return nil, establishErr
	}
	requestBytes, err := task.MarshalCanonical(winner)
	if err != nil {
		return nil, err
	}
	requestSHA := task.ComputeSHA256(requestBytes)

	journal = &GroupJournal{control: control, request: winner, requestSHA: requestSHA}
	closeOnError = false
	return journal, nil
}

func readGroupRequest(ctx context.Context, control *taskdir.ControlDir) (record contract.GroupRequestRecord, resultErr error) {
	if control == nil {
		return contract.GroupRequestRecord{}, os.ErrClosed
	}
	resultErr = control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		return tx.Read(groupRequestRecordName, &record)
	})
	return record, resultErr
}

func establishGroupRequest(ctx context.Context, control *taskdir.ControlDir, rootID string, binding task.SupervisorRef, expected contract.GroupRequestRecord) (record contract.GroupRequestRecord, resultErr error) {
	resultErr = withControlTransactionContext(ctx, control, func(tx *taskdir.ControlTransaction) error {
		var existing contract.GroupRequestRecord
		readErr := tx.Read(groupRequestRecordName, &existing)
		if readErr == nil {
			return acceptExistingGroupRequest(tx, rootID, binding, expected, existing, &record)
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		return createGroupRequest(tx, rootID, binding, expected, &record)
	})
	return record, resultErr
}

func acceptExistingGroupRequest(tx *taskdir.ControlTransaction, rootID string, binding task.SupervisorRef, expected, existing contract.GroupRequestRecord, record *contract.GroupRequestRecord) error {
	if validateErr := contract.ValidateGroupRequest(existing, rootID, binding); validateErr != nil {
		return fmt.Errorf("%w: invalid group request: %w", task.ErrEvidenceFault, validateErr)
	}
	if !contract.SameGroupRequest(existing, expected) {
		return fmt.Errorf("%w: group binding changed", task.ErrEvidenceFault)
	}
	// Put verifies the rooted byte-for-byte winner without replacing its
	// original launch provenance with this installation's executable paths.
	created, putErr := tx.Put(groupRequestRecordName, existing)
	if putErr != nil {
		return putErr
	}
	if created {
		return fmt.Errorf("%w: group request changed during replay", task.ErrEvidenceFault)
	}
	*record = existing
	return nil
}

func createGroupRequest(tx *taskdir.ControlTransaction, rootID string, binding task.SupervisorRef, expected contract.GroupRequestRecord, record *contract.GroupRequestRecord) error {
	created, putErr := tx.Put(groupRequestRecordName, expected)
	if putErr != nil {
		return putErr
	}
	if created {
		*record = expected
		return nil
	}

	// The control lock normally makes this path unreachable, but a preexisting
	// winner must still be validated before acceptance.
	var winner contract.GroupRequestRecord
	if winnerReadErr := tx.Read(groupRequestRecordName, &winner); winnerReadErr != nil {
		return winnerReadErr
	}
	if validateErr := contract.ValidateGroupRequest(winner, rootID, binding); validateErr != nil {
		return fmt.Errorf("%w: invalid group request winner: %w", task.ErrEvidenceFault, validateErr)
	}
	if !contract.SameGroupRequest(winner, expected) {
		return fmt.Errorf("%w: group binding changed", task.ErrEvidenceFault)
	}
	*record = winner
	return nil
}

// ClaimCreate records a durable create-once intent. Replays never yield a new
// permit, even when the existing guard bytes are identical.
func (g *GroupJournal) ClaimCreate(now time.Time) (*GroupPermit, error) {
	return g.ClaimCreateContext(context.Background(), now)
}

// ClaimCreateContext records the create-once intent within the caller's
// admission lifetime. A canceled or expired caller cannot publish a guard
// that would prevent a later, still-authorized reconciliation.
func (g *GroupJournal) ClaimCreateContext(ctx context.Context, now time.Time) (*GroupPermit, error) {
	if g == nil || g.control == nil {
		return nil, os.ErrClosed
	}
	var created bool
	err := withControlTransactionContext(ctx, g.control, func(tx *taskdir.ControlTransaction) error {
		var existing contract.GroupCreationRecord
		readErr := tx.Read(groupCreationRecordName, &existing)
		if readErr == nil {
			if validateErr := contract.ValidateGroupCreation(existing, g.requestSHA); validateErr != nil {
				return fmt.Errorf("%w: invalid persisted group creation: %w", task.ErrEvidenceFault, validateErr)
			}
			return nil
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			return groupClaimReadError(readErr)
		}

		createdAt, timeErr := contract.ValidGroupTime(now)
		if timeErr != nil {
			return timeErr
		}
		record := contract.GroupCreationRecord{
			SchemaVersion: task.SchemaVersion,
			RequestSHA256: g.requestSHA,
			CreatedAt:     createdAt,
		}
		if validateErr := contract.ValidateGroupCreation(record, g.requestSHA); validateErr != nil {
			return validateErr
		}
		var putErr error
		created, putErr = tx.Put(groupCreationRecordName, record)
		return putErr
	})
	if err != nil {
		return nil, err
	}
	if !created {
		return nil, task.ErrAlreadySubmitted
	}
	return &GroupPermit{state: newOneShot(), rootID: g.request.RootID, supervisor: g.request.Supervisor}, nil
}

func groupClaimReadError(readErr error) error {
	var pathErr *os.PathError
	if errors.As(readErr, &pathErr) || errors.Is(readErr, os.ErrClosed) {
		return readErr
	}
	return fmt.Errorf("%w: invalid persisted group creation: %w", task.ErrEvidenceFault, readErr)
}

// Observe validates a fresh supervisor snapshot and durably records its
// bounded positive facts. Existing observation bytes are never trusted in
// place of the supplied snapshot.
func (g *GroupJournal) Observe(snapshot pueue.QueueSnapshot) error {
	return g.ObserveContext(context.Background(), snapshot)
}

// ObserveContext is the context-aware form of Observe used by bounded
// admission paths.
func (g *GroupJournal) ObserveContext(ctx context.Context, snapshot pueue.QueueSnapshot) error {
	if g == nil || g.control == nil {
		return os.ErrClosed
	}
	if ctx == nil {
		return errors.New("nil observation context")
	}
	record, err := contract.GroupObservationForSnapshot(g.request, g.requestSHA, snapshot)
	if err != nil {
		return err
	}
	return withControlTransactionContext(ctx, g.control, func(tx *taskdir.ControlTransaction) error {
		_, putErr := tx.Put(groupObservationRecordName, record)
		return putErr
	})
}

// Name returns the immutable supervisor group name derived from the root ID.
func (g *GroupJournal) Name() string {
	if g == nil {
		return ""
	}
	return g.request.Name
}

// Digest returns the canonical request.json digest used by all group records.
func (g *GroupJournal) Digest() string {
	if g == nil {
		return ""
	}
	return g.requestSHA
}

// Request returns the immutable group request value.
func (g *GroupJournal) Request() contract.GroupRequestRecord {
	if g == nil {
		return contract.GroupRequestRecord{}
	}
	return g.request
}

// Close releases the group journal handle.
func (g *GroupJournal) Close() error {
	if g == nil || g.control == nil {
		return nil
	}
	return g.control.Close()
}

var _ pueue.InspectionGroupPermit = (*GroupPermit)(nil)
