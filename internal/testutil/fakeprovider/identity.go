package fakeprovider

import (
	"errors"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type identityObserver struct {
	mu       sync.Mutex
	state    *eventState
	parser   frameScanner
	record   func(task.SessionIdentity) error
	callback error
}

// NewIdentityObserver builds a bounded, observation-only stdout parser.
func NewIdentityObserver(taskID string, expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
	if err := task.ValidateTaskID(taskID); err != nil {
		return nil, err
	}
	if expected.Required && !validSessionID(expected.ID) {
		return nil, errors.New("required fixture session ID is invalid")
	}
	if record == nil {
		return nil, errors.New("nil fixture identity recorder")
	}
	state := newEventState(taskID, expected, nil)
	o := &identityObserver{state: state, record: record}
	o.parser = frameScanner{onFrame: o.frame, isFault: func() bool { return o.state.semantic != nil }}
	return o, nil
}

func (o *identityObserver) Observe(data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.parser.feed(data)
}

func (o *identityObserver) Complete() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.parser.finish()
	return o.callback
}

func (o *identityObserver) frame(frame []byte) {
	if frame == nil {
		o.state.latch("malformed_output")
		return
	}
	e, err := decodeEvent(frame)
	if err != nil {
		o.state.latch("malformed_output")
		return
	}
	if o.state.apply(e) && o.callback == nil && o.state.identity != nil {
		identity := *o.state.identity
		o.callback = o.record(identity)
	}
}

var _ execution.IdentityObserver = (*identityObserver)(nil)
