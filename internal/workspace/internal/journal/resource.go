package journal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

type inventoryRecord struct {
	SchemaVersion int    `json:"schema_version"`
	RootID        string `json:"root_id"`
	WorkspaceID   string `json:"workspace_id"`
	TaskID        string `json:"task_id"`
	RequestSHA256 string `json:"request_sha256"`
}

// Resource is the handle to the one authoritative workspace journal. Every
// mutation is serialized by the stable ControlDir lock; callers must perform
// external Git and lifetime work outside the short record transaction.
type Resource struct {
	control *taskdir.ControlDir
	store   *taskdir.Store
	request contract.ResourceRequest
}

// EstablishResource binds the intended task/request to exactly one workspace
// before provisioning. Concurrent identical admissions adopt the same winner.
func EstablishResource(ctx context.Context, store *taskdir.Store, request contract.ResourceRequest) (*Resource, error) {
	if store == nil {
		return nil, errors.New("nil task store")
	}
	if err := validateResourceRequest(request, store.RootID); err != nil {
		return nil, err
	}
	winner, err := establishResourceInventory(ctx, store, request)
	if err != nil {
		return nil, err
	}
	// The task-scoped registry winner is authoritative for a replay. The
	// caller's generated workspace ID is not a second identity source.
	request.WorkspaceID = winner.WorkspaceID

	control, err := store.OpenWorkspace(ctx, request.WorkspaceID, true)
	if err != nil {
		return nil, err
	}
	if err = control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if _, putErr := tx.Put("request.json", request); putErr != nil {
			return putErr
		}
		return tx.Verify("request.json", request)
	}); err != nil {
		return nil, errors.Join(err, control.Close())
	}
	return &Resource{control: control, store: store, request: request}, nil
}

// BindAdmission immutably binds an additional task admission to this
// workspace before the caller performs reservation or external work. The
// root task inventory remains the sole TaskID-to-workspace authority.
func (r *Resource) BindAdmission(ctx context.Context, taskID, requestSHA256 string) (resultErr error) {
	if r == nil || r.store == nil || r.control == nil {
		return errors.New("resource is not attached to a store")
	}
	if err := errors.Join(task.ValidateTaskID(taskID), task.ValidateSHA256(requestSHA256)); err != nil {
		return err
	}
	if err := validateResourceRequest(r.request, r.store.RootID); err != nil {
		return classifyStoredResourceRecordError(err)
	}
	registry, err := r.store.OpenWorkspaceRegistry(ctx, false)
	if err != nil {
		if task.RecordAbsent(err) {
			return errors.Join(task.ErrEvidenceFault, err)
		}
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, registry.Close()) }()
	resultErr = registry.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		workspace, readErr := readCanonicalWorkspaceInventoryTx(tx, r.request.WorkspaceID, r.request.RootID)
		if readErr != nil {
			return readErr
		}
		if !inventoryMatchesRequest(workspace, r.request) {
			return errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
		}
		_, bindErr := ensureTaskInventoryTx(tx, r.request.RootID, taskID, r.request.WorkspaceID, requestSHA256, true)
		return bindErr
	})
	return resultErr
}

func establishResourceInventory(ctx context.Context, store *taskdir.Store, request contract.ResourceRequest) (winner inventoryRecord, resultErr error) {
	registry, err := store.OpenWorkspaceRegistry(ctx, true)
	if err != nil {
		return winner, err
	}
	defer func() { resultErr = errors.Join(resultErr, registry.Close()) }()
	resultErr = registry.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		var err error
		winner, err = establishResourceInventoryTx(tx, request)
		return err
	})
	return winner, resultErr
}

func establishResourceInventoryTx(tx *taskdir.ControlTransaction, request contract.ResourceRequest) (winner inventoryRecord, resultErr error) {
	winner, resultErr = ensureTaskInventoryTx(tx, request.RootID, request.TaskID, request.WorkspaceID, request.RequestSHA256, false)
	if resultErr != nil {
		return winner, resultErr
	}
	workspaceName := workspaceInventoryName(winner.WorkspaceID)
	if _, resultErr = tx.Put(workspaceName, winner); resultErr != nil {
		return winner, resultErr
	}
	return winner, tx.Verify(workspaceName, winner)
}

func ensureTaskInventoryTx(tx *taskdir.ControlTransaction, rootID, taskID, workspaceID, requestSHA256 string, requireWorkspaceMatch bool) (winner inventoryRecord, resultErr error) {
	if err := errors.Join(task.ValidateRootID(rootID), task.ValidateTaskID(taskID), task.ValidateTaskID(workspaceID), task.ValidateSHA256(requestSHA256)); err != nil {
		return winner, err
	}
	name := taskInventoryName(taskID)
	readErr := tx.Read(name, &winner)
	if task.RecordAbsent(readErr) {
		winner = inventoryRecord{
			SchemaVersion: task.SchemaVersion,
			RootID:        rootID,
			WorkspaceID:   workspaceID,
			TaskID:        taskID,
			RequestSHA256: requestSHA256,
		}
		if _, resultErr = tx.Put(name, winner); resultErr != nil {
			return winner, resultErr
		}
	} else if readErr != nil {
		return winner, classifyResourceRecordReadError(readErr)
	}
	if resultErr = validateInventoryRecord(winner, rootID); resultErr != nil {
		return winner, classifyStoredResourceRecordError(resultErr)
	}
	if winner.TaskID != taskID {
		return winner, errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	if requireWorkspaceMatch && winner.WorkspaceID != workspaceID {
		return winner, errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	if winner.RequestSHA256 != requestSHA256 {
		if winner.WorkspaceID != workspaceID {
			return winner, errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
		}
		return winner, task.ErrRequestConflict
	}
	if resultErr = tx.Verify(name, winner); resultErr != nil {
		return winner, resultErr
	}
	return winner, nil
}

func inventoryMatchesRequest(record inventoryRecord, request contract.ResourceRequest) bool {
	return record.SchemaVersion == task.SchemaVersion && record.RootID == request.RootID && record.WorkspaceID == request.WorkspaceID && record.TaskID == request.TaskID && record.RequestSHA256 == request.RequestSHA256
}

func readInventoryRecordTx(tx *taskdir.ControlTransaction, name, rootID string) (record inventoryRecord, resultErr error) {
	if err := tx.Read(name, &record); err != nil {
		return record, requireResourceRecord(err)
	}
	if err := validateInventoryRecord(record, rootID); err != nil {
		return record, classifyStoredResourceRecordError(err)
	}
	if err := tx.Verify(name, record); err != nil {
		return record, err
	}
	return record, nil
}

func readCanonicalWorkspaceInventoryTx(tx *taskdir.ControlTransaction, workspaceID, rootID string) (workspace inventoryRecord, resultErr error) {
	workspace, resultErr = readInventoryRecordTx(tx, workspaceInventoryName(workspaceID), rootID)
	if resultErr != nil {
		return workspace, resultErr
	}
	if workspace.WorkspaceID != workspaceID {
		return workspace, errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	initial, resultErr := readInventoryRecordTx(tx, taskInventoryName(workspace.TaskID), rootID)
	if resultErr != nil {
		return workspace, resultErr
	}
	if initial != workspace {
		return workspace, errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	return workspace, nil
}

// LoadResource opens existing resource facts without creating operational
// state. The request record is the workspace-local identity authority.
func LoadResource(ctx context.Context, store *taskdir.Store, id string) (*Resource, error) {
	if store == nil {
		return nil, errors.New("nil task store")
	}
	control, err := store.OpenWorkspace(ctx, id, false)
	if err != nil {
		return nil, err
	}
	request, err := readResourceRecord[contract.ResourceRequest](ctx, control, "request.json")
	if err != nil {
		return nil, errors.Join(err, control.Close())
	}
	if err = validateResourceRequest(request, store.RootID); err != nil {
		return nil, errors.Join(classifyStoredResourceRecordError(err), control.Close())
	}
	if request.WorkspaceID != id {
		return nil, errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch, control.Close())
	}
	return &Resource{control: control, store: store, request: request}, nil
}

func validateResourceRequest(request contract.ResourceRequest, rootID string) error {
	if request.SchemaVersion != task.SchemaVersion || request.RootID != rootID || request.BudgetNanos <= 0 {
		return task.ErrIdentityMismatch
	}
	if err := errors.Join(task.ValidateRootID(rootID), task.ValidateTaskID(request.WorkspaceID), task.ValidateTaskID(request.TaskID), task.ValidateSHA256(request.RequestSHA256)); err != nil {
		return err
	}
	for _, path := range []string{request.SourceCwd, request.Container} {
		if err := contract.ValidateAbsoluteCleanPath(path); err != nil {
			return err
		}
	}
	if request.BaseRef != "" {
		if err := contract.ValidateRevision(request.BaseRef); err != nil {
			return task.ErrIdentityMismatch
		}
	}
	return nil
}

func (r *Resource) Request() contract.ResourceRequest { return r.request }

func (r *Resource) Close() error {
	if r == nil || r.control == nil {
		return os.ErrClosed
	}
	return r.control.Close()
}

// ResourceIDs lists committed root inventory entries, not guessed directories.
func ResourceIDs(ctx context.Context, store *taskdir.Store) (ids []string, resultErr error) {
	if store == nil {
		return nil, errors.New("nil task store")
	}
	registry, err := store.OpenWorkspaceRegistry(ctx, false)
	if task.RecordAbsent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, registry.Close()) }()

	err = registry.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		tasks, workspaces, readErr := readResourceInventory(ctx, registry, tx, store.RootID)
		if readErr != nil {
			return readErr
		}
		pairingErr := validateResourceInventoryPairing(tasks, workspaces)
		if pairingErr != nil {
			return pairingErr
		}
		for id := range workspaces {
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(ids)
	return ids, nil
}

func readResourceInventory(ctx context.Context, registry *taskdir.ControlDir, tx *taskdir.ControlTransaction, rootID string) (tasks, workspaces map[string]inventoryRecord, resultErr error) {
	names, err := registry.RecordNamesContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	tasks = make(map[string]inventoryRecord)
	workspaces = make(map[string]inventoryRecord)
	for _, name := range names {
		record, id, workspaceEntry, candidate, readErr := readInventoryEntry(tx, name, rootID)
		if readErr != nil {
			return nil, nil, readErr
		}
		if !candidate {
			continue
		}
		if workspaceEntry {
			workspaces[id] = record
		} else {
			tasks[id] = record
		}
	}
	return tasks, workspaces, nil
}

func readInventoryEntry(tx *taskdir.ControlTransaction, name, rootID string) (record inventoryRecord, id string, workspaceEntry, candidate bool, resultErr error) {
	workspaceID, workspaceCandidate, workspaceErr := parseWorkspaceInventoryName(name)
	taskID, taskCandidate, taskErr := parseTaskInventoryName(name)
	if workspaceErr != nil {
		return record, "", false, false, workspaceErr
	}
	if taskErr != nil {
		return record, "", false, false, taskErr
	}
	if workspaceCandidate && taskCandidate {
		return record, "", false, false, task.ErrEvidenceFault
	}
	if !workspaceCandidate && !taskCandidate {
		return record, "", false, false, nil
	}
	var readErr error
	record, readErr = readInventoryRecordTx(tx, name, rootID)
	if readErr != nil {
		return record, "", false, false, readErr
	}
	if workspaceCandidate {
		if record.WorkspaceID != workspaceID {
			return record, "", false, false, errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
		}
		return record, workspaceID, true, true, nil
	}
	if record.TaskID != taskID {
		return record, "", false, false, errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
	}
	return record, taskID, false, true, nil
}

func validateResourceInventoryPairing(tasks, workspaces map[string]inventoryRecord) error {
	for _, record := range workspaces {
		taskRecord, matched := tasks[record.TaskID]
		if !matched || taskRecord != record {
			return errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
		}
	}
	for _, record := range tasks {
		workspaceRecord, matched := workspaces[record.WorkspaceID]
		if !matched {
			return errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
		}
		if record.TaskID == workspaceRecord.TaskID && workspaceRecord != record {
			return errors.Join(task.ErrEvidenceFault, task.ErrIdentityMismatch)
		}
	}
	return nil
}

func taskInventoryName(id string) string      { return "task-" + id + ".json" }
func workspaceInventoryName(id string) string { return "workspace-" + id + ".json" }

func parseWorkspaceInventoryName(name string) (string, bool, error) {
	if !strings.HasPrefix(name, "workspace-") {
		return "", false, nil
	}
	if !strings.HasSuffix(name, ".json") {
		return "", true, task.ErrEvidenceFault
	}
	id := strings.TrimSuffix(strings.TrimPrefix(name, "workspace-"), ".json")
	if task.ValidateTaskID(id) != nil || workspaceInventoryName(id) != name {
		return "", true, task.ErrEvidenceFault
	}
	return id, true, nil
}

func parseTaskInventoryName(name string) (string, bool, error) {
	return validateIDName(name, "task-", ".json")
}

// ContainerPath is stable per root, independent of the chosen source checkout.
func ContainerPath(root, rootID string) string {
	return filepath.Join(filepath.Dir(root), "."+filepath.Base(root)+".workspaces", rootID)
}

// ScratchPath remains outside the provider checkout and inside owned state.
func ScratchPath(root, operationID string) string {
	return taskdir.WorkspaceArtifactDirectory(root, operationID)
}

func validateInventoryRecord(record inventoryRecord, rootID string) error {
	if record.SchemaVersion != task.SchemaVersion || record.RootID != rootID {
		return task.ErrIdentityMismatch
	}
	return errors.Join(task.ValidateRootID(record.RootID), task.ValidateTaskID(record.WorkspaceID), task.ValidateTaskID(record.TaskID), task.ValidateSHA256(record.RequestSHA256))
}

func validateIDName(name, prefix, suffix string) (string, bool, error) {
	if !strings.HasPrefix(name, prefix) {
		return "", false, nil
	}
	if !strings.HasSuffix(name, suffix) {
		return "", true, task.ErrEvidenceFault
	}
	id := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	if task.ValidateTaskID(id) != nil || prefix+id+suffix != name {
		return "", true, task.ErrEvidenceFault
	}
	return id, true, nil
}
