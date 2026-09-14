package contributorprovider

import (
	"errors"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// identityFactory observes the fixture's bounded envelope. Only the runner's
// callback owns persistence; this observer never opens protocol records.
func identityFactory(taskID string) func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
	return func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
		if err := task.ValidateTaskID(taskID); err != nil {
			return nil, err
		}
		if record == nil {
			return nil, errors.New("nil contributor session recorder")
		}
		if (expected.Required && !validSessionID(expected.ID)) || (!expected.Required && expected.ID != "") {
			return nil, errors.New("invalid contributor session expectation")
		}
		sessionID := "session-" + taskID
		if expected.Required {
			sessionID = expected.ID
		}
		return &identityObserver{taskID: taskID, sessionID: sessionID, record: record}, nil
	}
}

type identityObserver struct {
	mu        sync.Mutex
	taskID    string
	sessionID string
	record    func(task.SessionIdentity) error
	data      []byte
	overflow  bool
	completed bool
	err       error
}

func (o *identityObserver) Observe(data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.completed || o.overflow {
		return
	}
	if len(data) > MaxEnvelopeBytes-len(o.data) {
		o.overflow = true
		o.data = nil
		return
	}
	o.data = append(o.data, data...)
}

func (o *identityObserver) Complete() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.completed {
		return o.err
	}
	o.completed = true
	defer func() { o.data = nil }()
	var envelope Envelope
	// Invalid semantic evidence remains available to the sealed interpreter.
	// Only persistence failures are observer errors.
	if o.overflow || decodeEnvelope(o.data, &envelope) != nil || envelope.TaskID != o.taskID || envelope.SessionID != o.sessionID {
		return nil
	}
	o.err = o.record(task.SessionIdentity{Provider: Provider, ConversationID: envelope.SessionID})
	return o.err
}
