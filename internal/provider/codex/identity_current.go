package codex

import (
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ObserveRecord is the corrected live identity path. It receives a scoped
// random-access record view, validates only the thread authority fields, and
// never retains the record or imposes the historical line-size bound.
func (o *identityObserver) ObserveRecord(value providerjson.Value) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return o.callback
	}
	if o.callback != nil {
		return o.callback
	}
	typeFields, err := value.Object("type")
	if err != nil {
		return err
	}
	typeName, err := currentRequiredString(typeFields, "type", maxItemTypeBytes)
	if err != nil {
		return err
	}
	if typeName != eventThreadStarted {
		return nil
	}
	if o.seen {
		return fmt.Errorf("%w: duplicate Codex thread.started identity", task.ErrIdentityMismatch)
	}
	fields, err := value.Object("thread_id")
	if err != nil {
		return err
	}
	threadID, err := currentRequiredString(fields, "thread_id", maxItemIDBytes)
	if err != nil || !validThreadID(threadID) {
		return fmt.Errorf("%w: invalid Codex thread.started identity", task.ErrIdentityMismatch)
	}
	o.seen = true
	o.threadID = threadID
	if !identityMatchesExpectation(o.expected, threadID) {
		return nil
	}
	o.callback = o.record(task.SessionIdentity{Provider: o.provider, ConversationID: threadID})
	return o.callback
}

var _ execution.RecordIdentityObserver = (*identityObserver)(nil)
