package inspection

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

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

var (
	// ErrGroupMigrationRequired means an existing inspection journal belongs
	// to another supervisor and cannot be safely rebound automatically.
	ErrGroupMigrationRequired = errors.New("inspection group migration required")
	// ErrGroupMigrationBusy means the old supervisor still owns queued or
	// running inspection work, so a new generation must not be opened.
	ErrGroupMigrationBusy = errors.New("inspection group migration has active work")
	// ErrGroupMigrationUnsupported means the journal is bound to an external
	// supervisor that Delegate must never stop or migrate automatically.
	ErrGroupMigrationUnsupported = errors.New("inspection group migration is unsupported")
)

// GroupRequestRecord is the immutable root-scoped group binding. The group
// name and parallelism are derived by this package; callers cannot select a
// broader group or change its scheduling width through this record.
type GroupRequestRecord struct {
	SchemaVersion int                `json:"schema_version"`
	RootID        string             `json:"root_id"`
	Supervisor    task.SupervisorRef `json:"supervisor"`
	Name          string             `json:"name"`
	ParallelTasks uint64             `json:"parallel_tasks"`
}

// GroupMigrationRecord is an immutable prepare/complete marker for a
// supervisor generation change. The old group request remains the authority;
// these records only make a crash between supervisor transitions observable.
type GroupMigrationRecord struct {
	SchemaVersion   int                `json:"schema_version"`
	RootID          string             `json:"root_id"`
	LegacySHA256    string             `json:"legacy_sha256"`
	SuccessorSHA256 string             `json:"successor_sha256"`
	Successor       GroupRequestRecord `json:"successor"`
}

// GroupCreationRecord is the durable create-once intent for one supervisor
// group creation. Its existence never grants a second create permit.
type GroupCreationRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RequestSHA256 string `json:"request_sha256"`
	CreatedAt     string `json:"created_at"`
}

// GroupObservationRecord contains only fresh, bounded supervisor facts that
// prove the expected group is currently usable. There is intentionally no
// timestamp: a stored observation is never a substitute for a fresh snapshot.
type GroupObservationRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RequestSHA256 string `json:"request_sha256"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	ParallelTasks uint64 `json:"parallel_tasks"`
}

// GroupJournal owns the root-scoped inspection-group control directory.
// Records are outside ordinary task directories and contain no task payloads.
type GroupJournal struct {
	control    *taskdir.ControlDir
	request    GroupRequestRecord
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

func groupGenerationPublished(ctx context.Context, store *taskdir.Store, current, expected GroupRequestRecord) (bool, error) {
	seen := map[string]struct{}{groupRequestDigest(current): {}}
	for {
		if sameGroupRequest(current, expected) {
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

func openExistingGroupGeneration(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, expected GroupRequestRecord) (*GroupJournal, error) {
	generation := groupGeneration(binding)
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
	if !sameGroupRequest(request, expected) {
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
func ReadGroupRequest(ctx context.Context, store *taskdir.Store) (record GroupRequestRecord, resultErr error) {
	if store == nil {
		return GroupRequestRecord{}, errors.New("nil inspection store")
	}
	control, err := store.OpenInspectionGroupContext(ctx, false)
	if err != nil {
		return GroupRequestRecord{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, control.Close()) }()
	record, err = readGroupRequest(ctx, control)
	if errors.Is(err, os.ErrNotExist) {
		return GroupRequestRecord{}, fmt.Errorf("%w: inspection group request is missing", task.ErrEvidenceFault)
	}
	return record, err
}

// PrepareGroupMigration records that a legacy binding is being inspected.
// Replays accept the identical marker. A different successor is recorded
// under its own immutable pending name so a blocked migration cannot wedge a
// later upgrade attempt.
func PrepareGroupMigration(ctx context.Context, store *taskdir.Store, legacy, successor GroupRequestRecord) (resultErr error) {
	if store == nil {
		return errors.New("nil inspection store")
	}
	if err := validateStoredGroupRequest(legacy, store.RootID); err != nil {
		return err
	}
	if err := validateStoredGroupRequest(successor, store.RootID); err != nil {
		return err
	}
	control, err := openMigrationControl(ctx, store, legacy, true)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, control.Close()) }()
	record, err := migrationRecord(store.RootID, legacy, successor)
	if err != nil {
		return err
	}
	return control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		return prepareGroupMigrationLocked(tx, store.RootID, legacy, record)
	})
}

func prepareGroupMigrationLocked(tx *taskdir.ControlTransaction, rootID string, legacy GroupRequestRecord, record GroupMigrationRecord) error {
	existingRequest, err := ensureGroupRequest(tx, legacy)
	if err != nil {
		return err
	}
	if validateErr := validateStoredGroupRequest(existingRequest, rootID); validateErr != nil {
		return fmt.Errorf("%w: invalid migration generation request: %w", task.ErrEvidenceFault, validateErr)
	}
	if !sameGroupRequest(existingRequest, legacy) {
		return fmt.Errorf("%w: migration generation request changed", task.ErrEvidenceFault)
	}
	var existing GroupMigrationRecord
	err = tx.Read(groupMigrationRecordName, &existing)
	if errors.Is(err, os.ErrNotExist) {
		_, putErr := tx.Put(groupMigrationRecordName, record)
		return putErr
	}
	if err != nil {
		return err
	}
	if validateErr := validateMigrationRecord(existing, rootID, legacy); validateErr != nil {
		return fmt.Errorf("%w: invalid pending migration marker: %w", task.ErrEvidenceFault, validateErr)
	}
	if existing == record {
		return nil
	}
	_, putErr := tx.Put(pendingMigrationRecordName(record.SuccessorSHA256), record)
	return putErr
}

func ensureGroupRequest(tx *taskdir.ControlTransaction, legacy GroupRequestRecord) (GroupRequestRecord, error) {
	var existing GroupRequestRecord
	err := tx.Read(groupRequestRecordName, &existing)
	if !errors.Is(err, os.ErrNotExist) {
		return existing, err
	}
	created, putErr := tx.Put(groupRequestRecordName, legacy)
	if putErr != nil {
		return GroupRequestRecord{}, putErr
	}
	if created {
		return legacy, nil
	}
	err = tx.Read(groupRequestRecordName, &existing)
	if err != nil {
		return GroupRequestRecord{}, err
	}
	return existing, nil
}

// CompleteGroupMigration records that the old binding passed inspection and
// the successor generation may be used. It is written before successor group
// creation so a crash after the old endpoint is stopped remains recoverable.
func CompleteGroupMigration(ctx context.Context, store *taskdir.Store, legacy, successor GroupRequestRecord) (resultErr error) {
	if store == nil {
		return errors.New("nil inspection store")
	}
	if err := validateStoredGroupRequest(legacy, store.RootID); err != nil {
		return err
	}
	if err := validateStoredGroupRequest(successor, store.RootID); err != nil {
		return err
	}
	control, err := openMigrationControl(ctx, store, legacy, false)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, control.Close()) }()
	record, err := migrationRecord(store.RootID, legacy, successor)
	if err != nil {
		return err
	}
	return control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		return completeGroupMigrationLocked(tx, store.RootID, legacy, record)
	})
}

func completeGroupMigrationLocked(tx *taskdir.ControlTransaction, rootID string, legacy GroupRequestRecord, want GroupMigrationRecord) error {
	var completed GroupMigrationRecord
	err := tx.Read(groupMigrationDoneName, &completed)
	if err == nil {
		if validateErr := validateMigrationRecord(completed, rootID, legacy); validateErr != nil {
			return fmt.Errorf("%w: invalid completed group migration marker: %w", task.ErrEvidenceFault, validateErr)
		}
		if completed != want {
			return fmt.Errorf("%w: completed group migration edge changed", task.ErrEvidenceFault)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	prepared, err := exactPreparedGroupMigration(tx, rootID, legacy, want)
	if err != nil {
		return err
	}
	if !prepared {
		return fmt.Errorf("%w: exact group migration edge was not prepared", task.ErrEvidenceFault)
	}
	created, err := tx.Put(groupMigrationDoneName, want)
	if err != nil {
		return err
	}
	if !created {
		return fmt.Errorf("%w: completed group migration marker was not created", task.ErrEvidenceFault)
	}
	return nil
}

func exactPreparedGroupMigration(tx *taskdir.ControlTransaction, rootID string, legacy GroupRequestRecord, want GroupMigrationRecord) (bool, error) {
	var primary GroupMigrationRecord
	err := tx.Read(groupMigrationRecordName, &primary)
	if err == nil {
		if validateErr := validateMigrationRecord(primary, rootID, legacy); validateErr != nil {
			return false, fmt.Errorf("%w: invalid pending group migration marker: %w", task.ErrEvidenceFault, validateErr)
		}
		if primary == want {
			return true, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	var alternate GroupMigrationRecord
	err = tx.Read(pendingMigrationRecordName(want.SuccessorSHA256), &alternate)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if validateErr := validateMigrationRecord(alternate, rootID, legacy); validateErr != nil {
		return false, fmt.Errorf("%w: invalid exact pending group migration marker: %w", task.ErrEvidenceFault, validateErr)
	}
	return alternate == want, nil
}

// ReadGroupHead follows completed migration markers from the original journal
// to the latest known supervisor generation. The chain is append-only: each
// source generation owns its own marker, so later upgrades never overwrite an
// earlier migration record.
func ReadGroupHead(ctx context.Context, store *taskdir.Store) (head GroupRequestRecord, migrated bool, resultErr error) {
	if store == nil {
		return GroupRequestRecord{}, false, errors.New("nil inspection store")
	}
	head, err := readGroupRoot(ctx, store)
	if err != nil {
		return GroupRequestRecord{}, false, err
	}
	return followGroupMigrationChain(ctx, store, head)
}

func readGroupRoot(ctx context.Context, store *taskdir.Store) (GroupRequestRecord, error) {
	control, err := store.OpenInspectionGroupContext(ctx, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			retained, retainedErr := hasRetainedGroupGeneration(store)
			if retainedErr != nil {
				return GroupRequestRecord{}, retainedErr
			}
			if retained {
				return GroupRequestRecord{}, fmt.Errorf("%w: inspection group root journal is missing", task.ErrEvidenceFault)
			}
		}
		return GroupRequestRecord{}, err
	}
	head, readErr := readGroupRequest(ctx, control)
	closeErr := control.Close()
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			readErr = fmt.Errorf("%w: inspection group request is missing", task.ErrEvidenceFault)
		}
		return GroupRequestRecord{}, errors.Join(readErr, closeErr)
	}
	if closeErr != nil {
		return GroupRequestRecord{}, closeErr
	}
	if err = validateStoredGroupRequest(head, store.RootID); err != nil {
		return GroupRequestRecord{}, fmt.Errorf("%w: invalid group head: %w", task.ErrEvidenceFault, err)
	}
	return head, nil
}

func followGroupMigrationChain(ctx context.Context, store *taskdir.Store, head GroupRequestRecord) (GroupRequestRecord, bool, error) {
	migrated := false
	seen := map[string]struct{}{groupRequestDigest(head): {}}
	for {
		record, found, readMigrationErr := readMigrationRecord(ctx, store, head, groupMigrationDoneName)
		if readMigrationErr != nil {
			return GroupRequestRecord{}, false, readMigrationErr
		}
		if !found {
			return head, migrated, nil
		}
		if validateErr := validateMigrationRecord(record, store.RootID, head); validateErr != nil {
			return GroupRequestRecord{}, false, fmt.Errorf("%w: invalid group migration chain: %w", task.ErrEvidenceFault, validateErr)
		}
		digest := groupRequestDigest(record.Successor)
		if _, exists := seen[digest]; exists {
			return GroupRequestRecord{}, false, fmt.Errorf("%w: cyclic group migration chain", task.ErrEvidenceFault)
		}
		seen[digest] = struct{}{}
		head = record.Successor
		migrated = true
	}
}

// GroupHeadMatchesBinding reports whether an inspection journal exists and its
// current logical supervisor is the supplied binding. Installation provenance
// is intentionally ignored; callers use this distinction to reconcile a
// daemon identity drift without stealing a journal whose config or endpoint
// belongs to an older generation.
func GroupHeadMatchesBinding(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef) (present, matches bool, resultErr error) {
	if store == nil {
		return false, false, errors.New("nil inspection store")
	}
	expected, err := expectedGroupRequest(store, binding)
	if err != nil {
		return false, false, err
	}
	head, _, err := ReadGroupHead(ctx, store)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, sameGroupRequest(head, expected), nil
}

// GroupMigrationComplete reports whether the exact legacy-to-successor change
// has already passed its safety check.
func GroupMigrationComplete(ctx context.Context, store *taskdir.Store, legacy, successor GroupRequestRecord) (complete bool, resultErr error) {
	if store == nil {
		return false, errors.New("nil inspection store")
	}
	record, found, err := readMigrationRecord(ctx, store, legacy, groupMigrationDoneName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !found {
		return false, nil
	}
	want, err := migrationRecord(store.RootID, legacy, successor)
	if err != nil {
		return false, err
	}
	return record == want, nil
}

// HasCompletedGroupMigration reports whether this journal contains a valid
// completed migration marker. The legacy request remains for audit, so callers
// that only need read-only lifecycle status must distinguish retained history
// from pending work without contacting either supervisor.
func HasCompletedGroupMigration(ctx context.Context, store *taskdir.Store) (complete bool, resultErr error) {
	_, complete, resultErr = ReadGroupHead(ctx, store)
	return complete, resultErr
}

// GroupMigrationPending observes the current head's prepare marker through the
// journal owner. Alternate proposals always have a primary prepare marker.
func GroupMigrationPending(ctx context.Context, store *taskdir.Store) (bool, error) {
	successors, err := PreparedGroupMigrationSuccessors(ctx, store)
	return len(successors) != 0, err
}

// PreparedGroupMigrationSuccessors reads primary and alternate proposals for
// the current head. Selection cannot discard a valid later prepared edge.
func PreparedGroupMigrationSuccessors(ctx context.Context, store *taskdir.Store) (successors []task.SupervisorRef, resultErr error) {
	head, _, err := ReadGroupHead(ctx, store)
	if task.RecordAbsent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	control, err := openMigrationControl(ctx, store, head, false)
	if task.RecordAbsent(err) {
		return nil, nil // A completed edge may precede successor group creation.
	}
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, control.Close()) }()
	names, err := control.RecordNamesContext(ctx)
	if err != nil {
		return nil, err
	}
	primary := false
	for _, name := range names {
		if name == groupMigrationDoneName || !strings.HasPrefix(name, groupMigrationRecordPrefix) && name != groupMigrationRecordName {
			continue
		}
		record, readErr := readPreparedGroupMigration(control, store.RootID, head, name)
		if readErr != nil {
			return nil, readErr
		}
		primary = primary || name == groupMigrationRecordName
		successors = append(successors, record.Successor.Supervisor)
	}
	if len(successors) != 0 && !primary {
		return nil, fmt.Errorf("%w: alternate inspection proposal lacks primary marker", task.ErrEvidenceFault)
	}
	return successors, nil
}

func readPreparedGroupMigration(control *taskdir.ControlDir, rootID string, head GroupRequestRecord, name string) (GroupMigrationRecord, error) {
	var record GroupMigrationRecord
	if err := control.Read(name, &record); err != nil {
		return record, errors.Join(task.ErrEvidenceFault, err)
	}
	if err := validateMigrationRecord(record, rootID, head); err != nil {
		return record, fmt.Errorf("%w: invalid pending group migration: %w", task.ErrEvidenceFault, err)
	}
	if name != groupMigrationRecordName && name != pendingMigrationRecordName(record.SuccessorSHA256) {
		return record, fmt.Errorf("%w: inspection proposal name does not match its record", task.ErrEvidenceFault)
	}
	return record, nil
}

func readMigrationRecord(ctx context.Context, store *taskdir.Store, legacy GroupRequestRecord, name string) (record GroupMigrationRecord, found bool, resultErr error) {
	control, err := openMigrationControl(ctx, store, legacy, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return GroupMigrationRecord{}, false, nil
		}
		return GroupMigrationRecord{}, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, control.Close()) }()
	if err = control.Read(name, &record); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return GroupMigrationRecord{}, false, nil
		}
		return GroupMigrationRecord{}, false, err
	}
	return record, true, nil
}

func openMigrationControl(ctx context.Context, store *taskdir.Store, legacy GroupRequestRecord, create bool) (*taskdir.ControlDir, error) {
	if store == nil {
		return nil, errors.New("nil inspection store")
	}
	base, err := store.OpenInspectionGroupContext(ctx, false)
	if err == nil {
		existing, readErr := readGroupRequest(ctx, base)
		closeErr := base.Close()
		if readErr == nil && closeErr == nil && sameGroupRequest(existing, legacy) {
			return store.OpenInspectionGroupContext(ctx, create)
		}
		if errors.Is(readErr, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: inspection group root journal is missing", task.ErrEvidenceFault)
		}
		if readErr != nil {
			return nil, errors.Join(readErr, closeErr)
		}
		if closeErr != nil {
			return nil, closeErr
		}
	} else if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: inspection group root journal is missing", task.ErrEvidenceFault)
	} else {
		return nil, err
	}
	return store.OpenInspectionGroupGenerationContext(ctx, groupGeneration(legacy.Supervisor), create)
}

func hasRetainedGroupGeneration(store *taskdir.Store) (bool, error) {
	if store == nil {
		return false, errors.New("nil inspection store")
	}
	entries, err := os.ReadDir(store.Root)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "inspection-group-") || !validGroupGenerationSuffix(strings.TrimPrefix(name, "inspection-group-")) {
			continue
		}
		return true, nil
	}
	return false, nil
}

func validGroupGenerationSuffix(value string) bool {
	if len(value) != 16 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func migrationRecord(rootID string, legacy, successor GroupRequestRecord) (GroupMigrationRecord, error) {
	legacyBytes, legacyErr := task.MarshalCanonical(legacy)
	successorBytes, successorErr := task.MarshalCanonical(successor)
	if legacyErr != nil {
		return GroupMigrationRecord{}, legacyErr
	}
	if successorErr != nil {
		return GroupMigrationRecord{}, successorErr
	}
	return GroupMigrationRecord{
		SchemaVersion:   task.SchemaVersion,
		RootID:          rootID,
		LegacySHA256:    task.ComputeSHA256(legacyBytes),
		SuccessorSHA256: task.ComputeSHA256(successorBytes),
		Successor:       successor,
	}, nil
}

func pendingMigrationRecordName(successorSHA256 string) string {
	return groupMigrationRecordPrefix + successorSHA256[:32] + ".json"
}

func validateMigrationRecord(record GroupMigrationRecord, rootID string, legacy GroupRequestRecord) error {
	if record.SchemaVersion != task.SchemaVersion || record.RootID != rootID ||
		task.ValidateSHA256(record.LegacySHA256) != nil || task.ValidateSHA256(record.SuccessorSHA256) != nil {
		return errors.New("invalid migration marker")
	}
	if record.LegacySHA256 != groupRequestDigest(legacy) {
		return errors.New("migration marker legacy binding does not match its source")
	}
	if err := validateStoredGroupRequest(record.Successor, rootID); err != nil {
		return err
	}
	if record.SuccessorSHA256 != groupRequestDigest(record.Successor) {
		return errors.New("migration marker successor binding does not match its digest")
	}
	return nil
}

func groupRequestDigest(record GroupRequestRecord) string {
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return ""
	}
	return task.ComputeSHA256(data)
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

// OpenGroupWithMigration rebinds a stale inspection journal without changing
// its immutable records. A changed supervisor gets a deterministic successor
// directory only after check confirms that the old derived group has no active
// work. The check owns the bounded supervisor transition; callers must hold
// the state-root bootstrap lock while invoking it.
func OpenGroupWithMigration(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, check func(context.Context, GroupRequestRecord) error) (*GroupJournal, error) {
	if ctx == nil {
		return nil, errors.New("nil migration context")
	}
	expected, err := expectedGroupRequest(store, binding)
	if err != nil {
		return nil, err
	}
	legacy, migrated, err := ReadGroupHead(ctx, store)
	if errors.Is(err, os.ErrNotExist) {
		return openGroup(ctx, store, binding)
	}
	if err != nil {
		return nil, err
	}
	if sameGroupRequest(legacy, expected) {
		if !migrated {
			return openGroup(ctx, store, binding)
		}
		return openGroupGeneration(ctx, store, binding, expected)
	}
	if check == nil {
		return nil, fmt.Errorf("%w: changed supervisor identity", ErrGroupMigrationRequired)
	}
	checkErr := check(ctx, legacy)
	if checkErr != nil {
		return nil, checkErr
	}
	return openGroupGeneration(ctx, store, binding, expected)
}

func openGroupGeneration(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, expected GroupRequestRecord) (*GroupJournal, error) {
	generation := groupGeneration(binding)
	control, err := store.OpenInspectionGroupGenerationContext(ctx, generation, true)
	if err != nil {
		return nil, err
	}
	return openGroupControl(ctx, control, store.RootID, binding, expected)
}

func expectedGroupRequest(store *taskdir.Store, binding task.SupervisorRef) (GroupRequestRecord, error) {
	if store == nil {
		return GroupRequestRecord{}, errors.New("nil inspection store")
	}
	if rootErr := task.ValidateRootID(store.RootID); rootErr != nil {
		return GroupRequestRecord{}, rootErr
	}
	if bindingErr := task.ValidateFreshSupervisorRef(binding); bindingErr != nil {
		return GroupRequestRecord{}, bindingErr
	}
	expected := GroupRequestRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        store.RootID,
		Supervisor:    binding,
		Name:          groupForRoot(store.RootID),
		ParallelTasks: 1,
	}
	if requestErr := validateGroupRequest(expected, store.RootID, binding); requestErr != nil {
		return GroupRequestRecord{}, fmt.Errorf("%w: constructed group request: %w", task.ErrInvariantFault, requestErr)
	}
	return expected, nil
}

func openGroupControl(ctx context.Context, control *taskdir.ControlDir, rootID string, binding task.SupervisorRef, expected GroupRequestRecord) (journal *GroupJournal, resultErr error) {
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

func readGroupRequest(ctx context.Context, control *taskdir.ControlDir) (record GroupRequestRecord, resultErr error) {
	if control == nil {
		return GroupRequestRecord{}, os.ErrClosed
	}
	resultErr = control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		return tx.Read(groupRequestRecordName, &record)
	})
	return record, resultErr
}

func groupGeneration(binding task.SupervisorRef) string {
	identity := struct {
		ConfigPath           string `json:"config_path"`
		ConfigDigest         string `json:"config_digest"`
		ResolvedConfigSHA256 string `json:"resolved_config_sha256"`
		Endpoint             string `json:"endpoint"`
	}{
		ConfigPath:           binding.ConfigPath,
		ConfigDigest:         binding.ConfigDigest,
		ResolvedConfigSHA256: binding.ResolvedConfigSHA256,
		Endpoint:             binding.Endpoint,
	}
	bytes, err := task.MarshalCanonical(identity)
	if err != nil {
		return "0000000000000000"
	}
	return task.ComputeSHA256(bytes)[:16]
}

func establishGroupRequest(ctx context.Context, control *taskdir.ControlDir, rootID string, binding task.SupervisorRef, expected GroupRequestRecord) (record GroupRequestRecord, resultErr error) {
	resultErr = withControlTransactionContext(ctx, control, func(tx *taskdir.ControlTransaction) error {
		var existing GroupRequestRecord
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

func acceptExistingGroupRequest(tx *taskdir.ControlTransaction, rootID string, binding task.SupervisorRef, expected, existing GroupRequestRecord, record *GroupRequestRecord) error {
	if validateErr := validateGroupRequest(existing, rootID, binding); validateErr != nil {
		return fmt.Errorf("%w: invalid group request: %w", task.ErrEvidenceFault, validateErr)
	}
	if !sameGroupRequest(existing, expected) {
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

func createGroupRequest(tx *taskdir.ControlTransaction, rootID string, binding task.SupervisorRef, expected GroupRequestRecord, record *GroupRequestRecord) error {
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
	var winner GroupRequestRecord
	if winnerReadErr := tx.Read(groupRequestRecordName, &winner); winnerReadErr != nil {
		return winnerReadErr
	}
	if validateErr := validateGroupRequest(winner, rootID, binding); validateErr != nil {
		return fmt.Errorf("%w: invalid group request winner: %w", task.ErrEvidenceFault, validateErr)
	}
	if !sameGroupRequest(winner, expected) {
		return fmt.Errorf("%w: group binding changed", task.ErrEvidenceFault)
	}
	*record = winner
	return nil
}

func validateGroupRequest(record GroupRequestRecord, rootID string, binding task.SupervisorRef) error {
	if err := validateStoredGroupRequest(record, rootID); err != nil {
		return err
	}
	if !task.SameSupervisorIdentity(record.Supervisor, binding) {
		return task.ErrEvidenceFault
	}
	return nil
}

func validateStoredGroupRequest(record GroupRequestRecord, rootID string) error {
	if record.SchemaVersion != task.SchemaVersion {
		return fmt.Errorf("%w: group request schema %d", task.ErrUnsupportedSchema, record.SchemaVersion)
	}
	if err := task.ValidateRootID(record.RootID); err != nil {
		return err
	}
	if record.RootID != rootID {
		return task.ErrIdentityMismatch
	}
	if err := task.ValidateFreshSupervisorRef(record.Supervisor); err != nil {
		return err
	}
	if record.Name != groupForRoot(rootID) {
		return task.ErrIdentityMismatch
	}
	if record.ParallelTasks != 1 {
		return task.ErrInvalidEnum
	}
	return nil
}

func sameGroupRequest(left, right GroupRequestRecord) bool {
	return left.SchemaVersion == right.SchemaVersion &&
		left.RootID == right.RootID &&
		task.SameSupervisorIdentity(left.Supervisor, right.Supervisor) &&
		left.Name == right.Name &&
		left.ParallelTasks == right.ParallelTasks
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
		var existing GroupCreationRecord
		readErr := tx.Read(groupCreationRecordName, &existing)
		if readErr == nil {
			if validateErr := validateGroupCreation(existing, g.requestSHA); validateErr != nil {
				return fmt.Errorf("%w: invalid persisted group creation: %w", task.ErrEvidenceFault, validateErr)
			}
			return nil
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			return groupClaimReadError(readErr)
		}

		createdAt, timeErr := validGroupTime(now)
		if timeErr != nil {
			return timeErr
		}
		record := GroupCreationRecord{
			SchemaVersion: task.SchemaVersion,
			RequestSHA256: g.requestSHA,
			CreatedAt:     createdAt,
		}
		if validateErr := validateGroupCreation(record, g.requestSHA); validateErr != nil {
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

func validGroupTime(now time.Time) (string, error) {
	text := canonicalNow(now)
	if text == "" {
		return "", ErrInvalidAdmissionTime
	}
	parsed, err := canonicalTimestamp("group creation", text)
	if err != nil || !parsed.Equal(now.UTC()) {
		return "", ErrInvalidAdmissionTime
	}
	return text, nil
}

func validateGroupCreation(record GroupCreationRecord, requestSHA string) error {
	if record.SchemaVersion != task.SchemaVersion {
		return fmt.Errorf("%w: group creation schema %d", task.ErrUnsupportedSchema, record.SchemaVersion)
	}
	if record.RequestSHA256 != requestSHA {
		return task.ErrEvidenceFault
	}
	if err := task.ValidateSHA256(record.RequestSHA256); err != nil {
		return err
	}
	_, err := canonicalTimestamp("group creation", record.CreatedAt)
	return err
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
	record, err := groupObservationForSnapshot(g.request, g.requestSHA, snapshot)
	if err != nil {
		return err
	}
	return withControlTransactionContext(ctx, g.control, func(tx *taskdir.ControlTransaction) error {
		_, putErr := tx.Put(groupObservationRecordName, record)
		return putErr
	})
}

func groupObservationForSnapshot(request GroupRequestRecord, requestSHA string, snapshot pueue.QueueSnapshot) (GroupObservationRecord, error) {
	group, ok := snapshot.Groups[request.Name]
	if !ok || group.Status != "Running" || group.ParallelTasks != 1 {
		return GroupObservationRecord{}, pueue.ErrUnknown
	}
	record := GroupObservationRecord{
		SchemaVersion: task.SchemaVersion,
		RequestSHA256: requestSHA,
		Name:          request.Name,
		Status:        group.Status,
		ParallelTasks: group.ParallelTasks,
	}
	if err := validateGroupObservation(record, requestSHA, request.Name); err != nil {
		return GroupObservationRecord{}, err
	}
	return record, nil
}

func validateGroupObservation(record GroupObservationRecord, requestSHA, name string) error {
	if record.SchemaVersion != task.SchemaVersion {
		return fmt.Errorf("%w: group observation schema %d", task.ErrUnsupportedSchema, record.SchemaVersion)
	}
	if err := task.ValidateSHA256(record.RequestSHA256); err != nil {
		return err
	}
	if record.RequestSHA256 != requestSHA || record.Name != name || record.Status != "Running" || record.ParallelTasks != 1 {
		return task.ErrEvidenceFault
	}
	return nil
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
func (g *GroupJournal) Request() GroupRequestRecord {
	if g == nil {
		return GroupRequestRecord{}
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
