package journal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/inspection/contract"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// PrepareGroupMigration records that a legacy binding is being inspected.
// Replays accept the identical marker. A different successor is recorded
// under its own immutable pending name so a blocked migration cannot wedge a
// later upgrade attempt.
func PrepareGroupMigration(ctx context.Context, store *taskdir.Store, legacy, successor contract.GroupRequestRecord) (resultErr error) {
	if store == nil {
		return errors.New("nil inspection store")
	}
	if err := contract.ValidateStoredGroupRequest(legacy, store.RootID); err != nil {
		return err
	}
	if err := contract.ValidateStoredGroupRequest(successor, store.RootID); err != nil {
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

func prepareGroupMigrationLocked(tx *taskdir.ControlTransaction, rootID string, legacy contract.GroupRequestRecord, record contract.GroupMigrationRecord) error {
	existingRequest, err := ensureGroupRequest(tx, legacy)
	if err != nil {
		return err
	}
	if validateErr := contract.ValidateStoredGroupRequest(existingRequest, rootID); validateErr != nil {
		return fmt.Errorf("%w: invalid migration generation request: %w", task.ErrEvidenceFault, validateErr)
	}
	if !contract.SameGroupRequest(existingRequest, legacy) {
		return fmt.Errorf("%w: migration generation request changed", task.ErrEvidenceFault)
	}
	var existing contract.GroupMigrationRecord
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

func ensureGroupRequest(tx *taskdir.ControlTransaction, legacy contract.GroupRequestRecord) (contract.GroupRequestRecord, error) {
	var existing contract.GroupRequestRecord
	err := tx.Read(groupRequestRecordName, &existing)
	if !errors.Is(err, os.ErrNotExist) {
		return existing, err
	}
	created, putErr := tx.Put(groupRequestRecordName, legacy)
	if putErr != nil {
		return contract.GroupRequestRecord{}, putErr
	}
	if created {
		return legacy, nil
	}
	err = tx.Read(groupRequestRecordName, &existing)
	if err != nil {
		return contract.GroupRequestRecord{}, err
	}
	return existing, nil
}

// CompleteGroupMigration records that the old binding passed inspection and
// the successor generation may be used. It is written before successor group
// creation so a crash after the old endpoint is stopped remains recoverable.
func CompleteGroupMigration(ctx context.Context, store *taskdir.Store, legacy, successor contract.GroupRequestRecord) (resultErr error) {
	if store == nil {
		return errors.New("nil inspection store")
	}
	if err := contract.ValidateStoredGroupRequest(legacy, store.RootID); err != nil {
		return err
	}
	if err := contract.ValidateStoredGroupRequest(successor, store.RootID); err != nil {
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

func completeGroupMigrationLocked(tx *taskdir.ControlTransaction, rootID string, legacy contract.GroupRequestRecord, want contract.GroupMigrationRecord) error {
	var completed contract.GroupMigrationRecord
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

func exactPreparedGroupMigration(tx *taskdir.ControlTransaction, rootID string, legacy contract.GroupRequestRecord, want contract.GroupMigrationRecord) (bool, error) {
	var primary contract.GroupMigrationRecord
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

	var alternate contract.GroupMigrationRecord
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
func ReadGroupHead(ctx context.Context, store *taskdir.Store) (head contract.GroupRequestRecord, migrated bool, resultErr error) {
	if store == nil {
		return contract.GroupRequestRecord{}, false, errors.New("nil inspection store")
	}
	head, err := readGroupRoot(ctx, store)
	if err != nil {
		return contract.GroupRequestRecord{}, false, err
	}
	return followGroupMigrationChain(ctx, store, head)
}

func readGroupRoot(ctx context.Context, store *taskdir.Store) (contract.GroupRequestRecord, error) {
	control, err := store.OpenInspectionGroupContext(ctx, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			retained, retainedErr := hasRetainedGroupGeneration(store)
			if retainedErr != nil {
				return contract.GroupRequestRecord{}, retainedErr
			}
			if retained {
				return contract.GroupRequestRecord{}, fmt.Errorf("%w: inspection group root journal is missing", task.ErrEvidenceFault)
			}
		}
		return contract.GroupRequestRecord{}, err
	}
	head, readErr := readGroupRequest(ctx, control)
	closeErr := control.Close()
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			readErr = fmt.Errorf("%w: inspection group request is missing", task.ErrEvidenceFault)
		}
		return contract.GroupRequestRecord{}, errors.Join(readErr, closeErr)
	}
	if closeErr != nil {
		return contract.GroupRequestRecord{}, closeErr
	}
	if err = contract.ValidateStoredGroupRequest(head, store.RootID); err != nil {
		return contract.GroupRequestRecord{}, fmt.Errorf("%w: invalid group head: %w", task.ErrEvidenceFault, err)
	}
	return head, nil
}

func followGroupMigrationChain(ctx context.Context, store *taskdir.Store, head contract.GroupRequestRecord) (contract.GroupRequestRecord, bool, error) {
	migrated := false
	seen := map[string]struct{}{groupRequestDigest(head): {}}
	for {
		record, found, readMigrationErr := readMigrationRecord(ctx, store, head, groupMigrationDoneName)
		if readMigrationErr != nil {
			return contract.GroupRequestRecord{}, false, readMigrationErr
		}
		if !found {
			return head, migrated, nil
		}
		if validateErr := validateMigrationRecord(record, store.RootID, head); validateErr != nil {
			return contract.GroupRequestRecord{}, false, fmt.Errorf("%w: invalid group migration chain: %w", task.ErrEvidenceFault, validateErr)
		}
		digest := groupRequestDigest(record.Successor)
		if _, exists := seen[digest]; exists {
			return contract.GroupRequestRecord{}, false, fmt.Errorf("%w: cyclic group migration chain", task.ErrEvidenceFault)
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
	return true, contract.SameGroupRequest(head, expected), nil
}

// GroupMigrationComplete reports whether the exact legacy-to-successor change
// has already passed its safety check.
func GroupMigrationComplete(ctx context.Context, store *taskdir.Store, legacy, successor contract.GroupRequestRecord) (complete bool, resultErr error) {
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

func readPreparedGroupMigration(control *taskdir.ControlDir, rootID string, head contract.GroupRequestRecord, name string) (contract.GroupMigrationRecord, error) {
	var record contract.GroupMigrationRecord
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

func readMigrationRecord(ctx context.Context, store *taskdir.Store, legacy contract.GroupRequestRecord, name string) (record contract.GroupMigrationRecord, found bool, resultErr error) {
	control, err := openMigrationControl(ctx, store, legacy, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return contract.GroupMigrationRecord{}, false, nil
		}
		return contract.GroupMigrationRecord{}, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, control.Close()) }()
	if err = control.Read(name, &record); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return contract.GroupMigrationRecord{}, false, nil
		}
		return contract.GroupMigrationRecord{}, false, err
	}
	return record, true, nil
}

func openMigrationControl(ctx context.Context, store *taskdir.Store, legacy contract.GroupRequestRecord, create bool) (*taskdir.ControlDir, error) {
	if store == nil {
		return nil, errors.New("nil inspection store")
	}
	base, err := store.OpenInspectionGroupContext(ctx, false)
	if err == nil {
		existing, readErr := readGroupRequest(ctx, base)
		closeErr := base.Close()
		if readErr == nil && closeErr == nil && contract.SameGroupRequest(existing, legacy) {
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
	return store.OpenInspectionGroupGenerationContext(ctx, contract.GroupGeneration(legacy.Supervisor), create)
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

func migrationRecord(rootID string, legacy, successor contract.GroupRequestRecord) (contract.GroupMigrationRecord, error) {
	legacyBytes, legacyErr := task.MarshalCanonical(legacy)
	successorBytes, successorErr := task.MarshalCanonical(successor)
	if legacyErr != nil {
		return contract.GroupMigrationRecord{}, legacyErr
	}
	if successorErr != nil {
		return contract.GroupMigrationRecord{}, successorErr
	}
	return contract.GroupMigrationRecord{
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

func validateMigrationRecord(record contract.GroupMigrationRecord, rootID string, legacy contract.GroupRequestRecord) error {
	if record.SchemaVersion != task.SchemaVersion || record.RootID != rootID ||
		task.ValidateSHA256(record.LegacySHA256) != nil || task.ValidateSHA256(record.SuccessorSHA256) != nil {
		return errors.New("invalid migration marker")
	}
	if record.LegacySHA256 != groupRequestDigest(legacy) {
		return errors.New("migration marker legacy binding does not match its source")
	}
	if err := contract.ValidateStoredGroupRequest(record.Successor, rootID); err != nil {
		return err
	}
	if record.SuccessorSHA256 != groupRequestDigest(record.Successor) {
		return errors.New("migration marker successor binding does not match its digest")
	}
	return nil
}

func groupRequestDigest(record contract.GroupRequestRecord) string {
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return ""
	}
	return task.ComputeSHA256(data)
}

// OpenGroupWithMigration rebinds a stale inspection journal without changing
// its immutable records. A changed supervisor gets a deterministic successor
// directory only after check confirms that the old derived group has no active
// work. The check owns the bounded supervisor transition; callers must hold
// the state-root bootstrap lock while invoking it.
func OpenGroupWithMigration(ctx context.Context, store *taskdir.Store, binding task.SupervisorRef, check func(context.Context, contract.GroupRequestRecord) error) (*GroupJournal, error) {
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
	if contract.SameGroupRequest(legacy, expected) {
		if !migrated {
			return openGroup(ctx, store, binding)
		}
		return openGroupGeneration(ctx, store, binding, expected)
	}
	if check == nil {
		return nil, fmt.Errorf("%w: changed supervisor identity", contract.ErrGroupMigrationRequired)
	}
	checkErr := check(ctx, legacy)
	if checkErr != nil {
		return nil, checkErr
	}
	return openGroupGeneration(ctx, store, binding, expected)
}
