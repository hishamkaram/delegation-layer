package inspection

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	groupRequestRecordName     = "request.json"
	groupCreationRecordName    = "creation.json"
	groupObservationRecordName = "observation.json"
)

var (
	// ErrGroupMigrationRequired means an existing inspection journal belongs
	// to another supervisor and cannot be safely rebound automatically.
	ErrGroupMigrationRequired = errors.New("inspection group migration required")
	// ErrGroupMigrationBusy means the old supervisor still owns queued or
	// running inspection work, so a new generation must not be opened.
	ErrGroupMigrationBusy = errors.New("inspection group migration has active work")
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
	return openGroup(context.Background(), store, binding)
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
// work. The check must perform a bounded read-only observation and must not
// mutate either supervisor.
func OpenGroupWithMigration(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, check func(context.Context, GroupRequestRecord) error) (*GroupJournal, error) {
	if ctx == nil {
		return nil, errors.New("nil migration context")
	}
	expected, err := expectedGroupRequest(store, binding)
	if err != nil {
		return nil, err
	}

	legacy, err := store.OpenInspectionGroupContext(ctx, false)
	if errors.Is(err, os.ErrNotExist) {
		return openGroup(ctx, store, binding)
	}
	if err != nil {
		return nil, err
	}
	existing, readErr := readGroupRequest(ctx, legacy)
	closeErr := legacy.Close()
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			return openGroup(ctx, store, binding)
		}
		return nil, errors.Join(readErr, closeErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if validateErr := validateStoredGroupRequest(existing, store.RootID); validateErr != nil {
		return nil, fmt.Errorf("%w: invalid group request: %w", task.ErrEvidenceFault, validateErr)
	}
	if sameGroupRequest(existing, expected) {
		return openGroup(ctx, store, binding)
	}
	if check == nil {
		return nil, fmt.Errorf("%w: changed supervisor identity", ErrGroupMigrationRequired)
	}
	checkErr := check(ctx, existing)
	if checkErr != nil {
		return nil, checkErr
	}
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
	if g == nil || g.control == nil {
		return os.ErrClosed
	}
	group, ok := snapshot.Groups[g.request.Name]
	if !ok || group.Status != "Running" || group.ParallelTasks != 1 {
		return pueue.ErrUnknown
	}
	record := GroupObservationRecord{
		SchemaVersion: task.SchemaVersion,
		RequestSHA256: g.requestSHA,
		Name:          g.request.Name,
		Status:        group.Status,
		ParallelTasks: group.ParallelTasks,
	}
	if err := validateGroupObservation(record, g.requestSHA, g.request.Name); err != nil {
		return err
	}
	return withControlTransaction(g.control, func(tx *taskdir.ControlTransaction) error {
		_, putErr := tx.Put(groupObservationRecordName, record)
		return putErr
	})
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
