package journal

import (
	"context"
	"os"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection/contract"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const (
	stopRequestRecordName     = "stop-request.json"
	stopReplyRecordName       = "stop-reply.json"
	stopObservationRecordName = "stop-observation.json"
)

// StopPermit represents a durable one-use stop intent for an already observed
// exact target. Its identity, target, and supervisor binding are copied from
// the saved operation at claim time; it cannot grant a new submission or
// native start.
type StopPermit struct {
	state         *oneShot
	rootID        string
	taskID        string
	queueGroup    string
	numericTaskID int64
	supervisor    task.SupervisorRef
}

// ConsumeInspectionStop marks the stop permit consumed before an external
// supervisor mutation. Its name is action-specific so this permit cannot
// satisfy pueue's group or submission interface.
func (p *StopPermit) ConsumeInspectionStop() error {
	if p == nil {
		return task.ErrInvalidPermit
	}
	return p.state.consume()
}

// InspectionStopRootID returns the root identity saved in the operation.
func (p *StopPermit) InspectionStopRootID() string {
	if p == nil {
		return ""
	}
	return p.rootID
}

// InspectionStopTaskID returns the task identity saved in the operation.
func (p *StopPermit) InspectionStopTaskID() string {
	if p == nil {
		return ""
	}
	return p.taskID
}

// InspectionQueueGroup returns the queue group saved in the operation.
func (p *StopPermit) InspectionQueueGroup() string {
	if p == nil {
		return ""
	}
	return p.queueGroup
}

// InspectionStopNumericTaskID returns a defensive copy of the saved numeric
// target so callers cannot mutate permit state after validation.
func (p *StopPermit) InspectionStopNumericTaskID() *int64 {
	if p == nil {
		return nil
	}
	id := p.numericTaskID
	return &id
}

// InspectionStopSupervisor returns the supervisor binding saved in the
// operation. The value is returned by copy.
func (p *StopPermit) InspectionStopSupervisor() task.SupervisorRef {
	if p == nil {
		return task.SupervisorRef{}
	}
	return p.supervisor
}

// Consume retains the journal-level one-shot API for callers that consume a
// stop permit directly. The pueue boundary uses the action-specific method.
func (p *StopPermit) Consume() error {
	return p.ConsumeInspectionStop()
}

// ClaimStop performs no external operation and is safe under the budget owner's
// completion arbitration. Target discovery must have finished before entry.
func (o *Operation) ClaimStop(deadline time.Time) (*StopPermit, contract.StopRequestRecord, error) {
	if o == nil || o.control == nil || !deadline.Equal(o.deadline) {
		return nil, contract.StopRequestRecord{}, task.ErrEvidenceFault
	}
	receipt, err := o.Receipt()
	if err != nil {
		return nil, contract.StopRequestRecord{}, err
	}
	record := contract.StopRequestRecord{
		SchemaVersion: task.SchemaVersion, RequestSHA256: o.digest, NumericTaskID: *receipt.NumericTaskID,
		Supervisor: o.request.Binding.Supervisor, Deadline: o.request.Deadline,
	}
	var created bool
	err = withControlTransaction(o.control, func(tx *taskdir.ControlTransaction) error {
		var putErr error
		created, putErr = tx.Put(stopRequestRecordName, record)
		return putErr
	})
	if err != nil {
		return nil, contract.StopRequestRecord{}, err
	}
	if !created {
		return nil, record, contract.ErrStopAlreadyClaimed
	}
	return &StopPermit{
		state:         newOneShot(),
		rootID:        o.request.RootID,
		taskID:        o.request.IdentityID(),
		queueGroup:    o.request.Group,
		numericTaskID: record.NumericTaskID,
		supervisor:    o.request.Binding.Supervisor,
	}, record, nil
}

var _ pueue.InspectionStopPermit = (*StopPermit)(nil)

func (o *Operation) readStopRequestContext(ctx context.Context) (contract.StopRequestRecord, string, error) {
	if o == nil || o.control == nil {
		return contract.StopRequestRecord{}, "", os.ErrClosed
	}
	var record contract.StopRequestRecord
	if err := withControlTransactionContext(ctx, o.control, func(tx *taskdir.ControlTransaction) error {
		return tx.Read(stopRequestRecordName, &record)
	}); err != nil {
		return contract.StopRequestRecord{}, "", err
	}
	receipt, err := o.ReceiptContext(ctx)
	if err != nil {
		return contract.StopRequestRecord{}, "", err
	}
	if record.SchemaVersion != task.SchemaVersion || record.RequestSHA256 != o.digest || record.Supervisor != o.request.Binding.Supervisor || record.Deadline != o.request.Deadline || record.NumericTaskID != *receipt.NumericTaskID {
		return contract.StopRequestRecord{}, "", task.ErrEvidenceFault
	}
	data, err := task.MarshalCanonical(record)
	if err != nil {
		return contract.StopRequestRecord{}, "", task.ErrEvidenceFault
	}
	return record, task.ComputeSHA256(data), nil
}

// RecordStopReply records an acknowledgment only. It never supplies termination
// evidence. An uncertain reply remains absent rather than defaulting to false.
func (o *Operation) RecordStopReply(id int64, action string, acknowledged bool) error {
	return o.RecordStopReplyContext(context.Background(), id, action, acknowledged)
}

// RecordStopReplyContext records an acknowledgment while honoring the caller's
// lifetime for both the bound request read and the durable write.
func (o *Operation) RecordStopReplyContext(ctx context.Context, id int64, action string, acknowledged bool) error {
	request, digest, err := o.readStopRequestContext(ctx)
	if err != nil {
		return err
	}
	if request.NumericTaskID != id || (action != "remove" && action != "kill" && action != "terminate") {
		return task.ErrEvidenceFault
	}
	_, err = o.control.PutContext(ctx, stopReplyRecordName, contract.StopReplyRecord{
		SchemaVersion: task.SchemaVersion, StopSHA256: digest, NumericTaskID: id, Action: action, Acknowledged: acknowledged,
	})
	return err
}

// RecordStopObservation is called only after separate supervisor reconciliation
// observes the exact target ended. A stop acknowledgment cannot call this path.
func (o *Operation) RecordStopObservation(id int64) error {
	return o.RecordStopObservationContext(context.Background(), id)
}

// RecordStopObservationContext records an independently observed termination
// while honoring the caller's lifetime for both the bound request read and the
// durable write.
func (o *Operation) RecordStopObservationContext(ctx context.Context, id int64) error {
	request, digest, err := o.readStopRequestContext(ctx)
	if err != nil {
		return err
	}
	if request.NumericTaskID != id {
		return task.ErrEvidenceFault
	}
	_, err = o.control.PutContext(ctx, stopObservationRecordName, contract.StopObservationRecord{
		SchemaVersion: task.SchemaVersion, StopSHA256: digest, NumericTaskID: id, State: "ended",
	})
	return err
}
