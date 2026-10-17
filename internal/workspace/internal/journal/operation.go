package journal

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/workspace/contract"
)

// Operation binds a workspace helper to the shared control-lifetime journal.
// It owns storage handles, not process-lifetime decisions.
type Operation struct {
	*taskdir.ControlLifetime
	control  *taskdir.ControlDir
	request  contract.OperationRequest
	digest   string
	deadline time.Time
}

// OpenOperation creates or verifies the exact request. It never renews a
// deadline, overwrites an existing winner, or grants process-start authority.
func OpenOperation(ctx context.Context, store *taskdir.Store, request contract.OperationRequest) (operation *Operation, resultErr error) {
	deadline, err := contract.ValidateOperation(request, store.RootID)
	if err != nil {
		return nil, err
	}
	control, err := store.OpenWorkspaceOperation(ctx, request.OperationID, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		if operation == nil {
			resultErr = errors.Join(resultErr, control.Close())
		}
	}()
	err = control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		_, putErr := tx.Put("request.json", request)
		if putErr != nil {
			return putErr
		}
		return tx.Verify("request.json", request)
	})
	if err != nil {
		return nil, err
	}
	return newOperation(control, request, deadline)
}

// LoadOperation opens only an existing canonical operation request.
func LoadOperation(ctx context.Context, store *taskdir.Store, id string) (operation *Operation, resultErr error) {
	control, err := store.OpenWorkspaceOperation(ctx, id, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		if operation == nil {
			resultErr = errors.Join(resultErr, control.Close())
		}
	}()
	var request contract.OperationRequest
	var deadline time.Time
	err = control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if readErr := tx.Read("request.json", &request); readErr != nil {
			return readErr
		}
		var validationErr error
		deadline, validationErr = contract.ValidateOperation(request, store.RootID)
		if validationErr != nil || request.OperationID != id {
			return task.ErrIdentityMismatch
		}
		return tx.Verify("request.json", request)
	})
	if err != nil {
		return nil, err
	}
	return newOperation(control, request, deadline)
}

func newOperation(control *taskdir.ControlDir, request contract.OperationRequest, deadline time.Time) (*Operation, error) {
	data, err := task.MarshalCanonical(request)
	if err != nil {
		return nil, err
	}
	digest := task.ComputeSHA256(data)
	journal, err := taskdir.NewControlLifetime(control, request.RootID, request.OperationID, task.LifetimeOwnerWorkspace, digest, deadline)
	if err != nil {
		return nil, err
	}
	request.Arguments = slices.Clone(request.Arguments)
	return &Operation{ControlLifetime: journal, control: control, request: request, digest: digest, deadline: deadline}, nil
}

func (o *Operation) Request() contract.OperationRequest {
	request := o.request
	request.Arguments = slices.Clone(request.Arguments)
	return request
}

func (o *Operation) Digest() string      { return o.digest }
func (o *Operation) Deadline() time.Time { return o.deadline }
func (o *Operation) Close() error        { return o.control.Close() }

type operationGuard struct {
	SchemaVersion int    `json:"schema_version"`
	RequestSHA256 string `json:"request_sha256"`
}

// Claim records a create-once side-effect guard under the operation lock.
func (o *Operation) Claim(ctx context.Context, name string) (bool, error) {
	var created bool
	err := o.WithLifetimeLaunchPermissionContext(ctx, func(tx *taskdir.ControlTransaction) error {
		var putErr error
		created, putErr = tx.Put(name, operationGuard{SchemaVersion: task.SchemaVersion, RequestSHA256: o.digest})
		return putErr
	})
	return created, err
}

func (o *Operation) RecordReceipt(ctx context.Context, id int64) error {
	if id < 0 {
		return task.ErrIdentityMismatch
	}
	record := contract.Receipt{SchemaVersion: task.SchemaVersion, RequestSHA256: o.digest, NumericTaskID: id}
	return o.WithLifetimeLaunchPermissionOrReplayContext(ctx, func(tx *taskdir.ControlTransaction) error {
		var existing contract.Receipt
		if err := tx.Read("receipt.json", &existing); err != nil {
			return err
		}
		return tx.Verify("receipt.json", record)
	}, func(tx *taskdir.ControlTransaction) error {
		_, err := tx.Put("receipt.json", record)
		return err
	})
}

func (o *Operation) TryAcquireLifetimeOwner(ctx context.Context) (*taskdir.LifetimeOwnerLease, error) {
	return o.control.TryAcquireLifetimeOwnerContext(ctx)
}

func (o *Operation) Receipt(ctx context.Context) (result contract.Receipt, resultErr error) {
	resultErr = o.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if err := tx.Read("receipt.json", &result); err != nil {
			return err
		}
		if result.SchemaVersion != task.SchemaVersion || result.RequestSHA256 != o.digest || result.NumericTaskID < 0 {
			return task.ErrIdentityMismatch
		}
		return tx.Verify("receipt.json", result)
	})
	return result, resultErr
}

func (o *Operation) RecordResult(ctx context.Context, result contract.OperationResult) error {
	if err := o.validateResult(result); err != nil {
		return err
	}
	_, err := o.control.PutContext(ctx, "result.json", result)
	return err
}

func (o *Operation) Result(ctx context.Context) (result contract.OperationResult, resultErr error) {
	var absent error
	resultErr = o.control.WithLock(ctx, func(tx *taskdir.ControlTransaction) error {
		if err := tx.Read("result.json", &result); err != nil {
			if task.RecordAbsent(err) {
				absent = err
				return nil
			}
			return classifyResourceRecordReadError(err)
		}
		if err := o.validateResult(result); err != nil {
			return classifyStoredResourceRecordError(err)
		}
		return tx.Verify("result.json", result)
	})
	if resultErr != nil {
		return result, resultErr
	}
	return result, absent
}

func (o *Operation) validateResult(result contract.OperationResult) error {
	if result.SchemaVersion != task.SchemaVersion || result.OperationID != o.request.OperationID || result.RequestSHA256 != o.digest || (result.Status != "succeeded" && result.Status != "failed") {
		return task.ErrIdentityMismatch
	}
	if err := validateOperationResultVerdict(result.Status, result.ReasonCode); err != nil {
		return err
	}
	if o.request.Kind == contract.OperationVerify && result.Status == "succeeded" {
		if result.CommandExit == nil || *result.CommandExit < 0 {
			return task.ErrIdentityMismatch
		}
	} else if result.CommandExit != nil {
		return task.ErrIdentityMismatch
	}
	return o.validateResultArtifacts(result.Status, result.Artifacts)
}

const operationReasonCodeLimit = 64

func validateOperationResultVerdict(status, reasonCode string) error {
	switch status {
	case "succeeded":
		if reasonCode != "" {
			return task.ErrIdentityMismatch
		}
	case "failed":
		if err := validateOperationReasonCode(reasonCode); err != nil {
			return err
		}
	default:
		return task.ErrIdentityMismatch
	}
	return nil
}

// validateOperationReasonCode accepts a bounded machine-readable code instead
// of a closed list so adding a production failure branch does not invalidate
// this durable boundary before its consumers are updated.
func validateOperationReasonCode(reasonCode string) error {
	if len(reasonCode) == 0 || len(reasonCode) > operationReasonCodeLimit || reasonCode[0] < 'a' || reasonCode[0] > 'z' || reasonCode[len(reasonCode)-1] == '_' {
		return task.ErrIdentityMismatch
	}
	for index := 1; index < len(reasonCode); index++ {
		character := reasonCode[index]
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return task.ErrIdentityMismatch
		}
	}
	return nil
}

func (o *Operation) validateResultArtifacts(status string, artifacts []contract.Artifact) error {
	expected := operationResultArtifactNames(o.request.Kind)
	allowed := expected
	if status == "failed" {
		allowed = append(slices.Clone(expected), contract.FailureDiagnosticName)
	}
	seen := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		if err := validateOperationArtifact(o.request.Kind, artifact); err != nil {
			return err
		}
		if !slices.Contains(allowed, artifact.Name) || slices.Contains(seen, artifact.Name) {
			return task.ErrIdentityMismatch
		}
		seen = append(seen, artifact.Name)
	}
	if status == "succeeded" && len(seen) != len(expected) {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateOperationArtifact(kind string, artifact contract.Artifact) error {
	if err := contract.ValidateArtifact(artifact); err != nil {
		return err
	}
	if kind == contract.OperationVerify || artifact.Name == contract.FailureDiagnosticName {
		return validateDiagnosticArtifact(artifact)
	}
	if kind == contract.OperationCapture && artifact.Truncated {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validateDiagnosticArtifact(artifact contract.Artifact) error {
	if artifact.Omitted != "" || (artifact.Truncated && artifact.Length != contract.ArtifactLimit) {
		return task.ErrIdentityMismatch
	}
	return nil
}

func operationResultArtifactNames(kind string) []string {
	switch kind {
	case contract.OperationCapture:
		return []string{"diff.patch", "name-status.z"}
	case contract.OperationVerify:
		return []string{"stdout", "stderr"}
	default:
		return nil
	}
}

// SubmissionPermit is consumed exactly once by the existing scheduling edge.
type SubmissionPermit struct {
	mu        sync.Mutex
	operation *Operation
	consumed  bool
}

func (o *Operation) SubmissionPermit(ctx context.Context) (*SubmissionPermit, error) {
	created, err := o.Claim(ctx, "submission.json")
	if err != nil {
		return nil, err
	}
	if !created {
		return nil, os.ErrExist
	}
	return &SubmissionPermit{operation: o}, nil
}

func (p *SubmissionPermit) ConsumeInspectionSubmission() error {
	if p == nil || p.operation == nil {
		return task.ErrInvalidPermit
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.consumed {
		return task.ErrInvalidPermit
	}
	p.consumed = true
	return nil
}

func (p *SubmissionPermit) InspectionSubmissionRootID() string {
	if p == nil || p.operation == nil {
		return ""
	}
	return p.operation.request.RootID
}

func (p *SubmissionPermit) InspectionSubmissionTaskID() string {
	if p == nil || p.operation == nil {
		return ""
	}
	return p.operation.request.OperationID
}

func (p *SubmissionPermit) InspectionQueueGroup() string { return "default" }

func (p *SubmissionPermit) InspectionSubmissionSupervisor() task.SupervisorRef {
	if p == nil || p.operation == nil {
		return task.SupervisorRef{}
	}
	return p.operation.request.Supervisor
}
