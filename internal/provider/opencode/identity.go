package opencode

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// NewIdentityObserver builds a runner-owned bounded observer for OpenCode's
// sessionID field. The first valid event records a fresh session; a resumed
// task records only the exact expected session.
func NewIdentityObserver(taskID string, expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
	if err := task.ValidateTaskID(taskID); err != nil {
		return nil, err
	}
	if expected.Required && !validSessionID(expected.ID) {
		return nil, errors.New("required OpenCode session ID is invalid")
	}
	if !expected.Required && expected.ID != "" {
		return nil, errors.New("unexpected OpenCode continuation session ID")
	}
	if record == nil {
		return nil, errors.New("nil OpenCode identity recorder")
	}
	observer := &identityObserver{expected: expected, record: record}
	observer.framer = commonprovider.NewJSONLFramer(maxEventLineBytes, observer.processLine)
	return observer, nil
}

type identityObserver struct {
	mu       sync.Mutex
	expected task.SessionExpectation
	record   func(task.SessionIdentity) error
	framer   *commonprovider.JSONLFramer
	session  string
	seen     bool
	done     bool
	callback error
}

func (o *identityObserver) Observe(data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return
	}
	o.framer.Feed(data)
}

func (o *identityObserver) Complete() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return o.callback
	}
	o.framer.Finish()
	o.done = true
	return o.callback
}

func (o *identityObserver) processLine(line []byte) error {
	if len(bytes.TrimSpace(line)) == 0 {
		return errors.New("blank OpenCode identity event line")
	}
	if !utf8.Valid(line) {
		return errors.New("OpenCode identity event is not valid UTF-8")
	}
	event, err := decodeEvent(line)
	if err != nil {
		return fmt.Errorf("invalid OpenCode identity event: %w", err)
	}
	sessionID, err := requiredSessionID(event.fields)
	if err != nil {
		return err
	}
	if o.seen {
		if o.session != sessionID {
			return errors.New("conflicting OpenCode session identity")
		}
		return nil
	}
	o.seen = true
	o.session = sessionID
	if sessionMatchesExpectation(o.expected, sessionID) {
		o.callback = o.record(task.SessionIdentity{Provider: Provider, ConversationID: sessionID})
		if o.callback != nil {
			o.callback = fmt.Errorf("recording OpenCode session identity: %w", o.callback)
		}
	}
	return nil
}

func sessionMatchesExpectation(expected task.SessionExpectation, sessionID string) bool {
	if !validSessionID(sessionID) {
		return false
	}
	if expected.Required {
		return expected.ID == sessionID
	}
	return expected.ID == ""
}

var _ execution.IdentityObserver = (*identityObserver)(nil)
