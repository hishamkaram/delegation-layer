package pi

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

// NewIdentityObserver builds a bounded observer for Pi's session header. A
// fresh task records the first valid session UUID; continuation records it
// only when it exactly matches the predecessor expectation.
func NewIdentityObserver(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
	if expected.Required {
		if !validSessionID(expected.ID) {
			return nil, errors.New("required Pi session ID is invalid")
		}
	} else if expected.ID != "" {
		return nil, errors.New("unexpected Pi continuation session ID")
	}
	if record == nil {
		return nil, errors.New("nil Pi identity recorder")
	}
	observer := &identityObserver{expected: expected, record: record}
	observer.framer = commonprovider.NewJSONLFramer(maxEventLineBytes, observer.processLine)
	return observer, nil
}

type identityObserver struct {
	mu          sync.Mutex
	expected    task.SessionExpectation
	record      func(task.SessionIdentity) error
	framer      *commonprovider.JSONLFramer
	callbackErr error
	sessionID   string
	seen        bool
	done        bool
	completeErr error
}

// Observe accepts capture chunks while stdout remains live. It retains only
// one bounded JSONL line and never stops the provider capture worker.
func (o *identityObserver) Observe(data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return
	}
	o.framer.Feed(data)
}

// Complete flushes a final unterminated line and freezes observation. Parser
// semantic faults belong to the sealed output predicate; only an identity
// recorder failure is returned to the execution runner.
func (o *identityObserver) Complete() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return o.completeErr
	}
	o.framer.Finish()
	o.done = true
	o.completeErr = o.callbackErr
	return o.completeErr
}

func (o *identityObserver) processLine(line []byte) error {
	if len(bytes.TrimSpace(line)) == 0 || !utf8.Valid(line) {
		return errors.New("invalid Pi identity event line")
	}
	event, err := decodeEvent(line)
	if err != nil {
		return fmt.Errorf("invalid Pi identity event: %w", err)
	}
	if event.typeName != "session" {
		return nil
	}
	if o.seen {
		return errors.New("multiple Pi session events")
	}
	sessionID, err := requiredString(event.fields, "id")
	if err != nil || !validSessionID(sessionID) {
		return errors.New("pi session id is invalid")
	}
	o.seen = true
	o.sessionID = sessionID
	if identityMatchesExpectation(o.expected, sessionID) {
		o.callbackErr = o.record(task.SessionIdentity{Provider: Provider, ConversationID: sessionID})
		if o.callbackErr != nil {
			o.callbackErr = fmt.Errorf("recording Pi session identity: %w", o.callbackErr)
		}
	}
	return nil
}

func identityMatchesExpectation(expected task.SessionExpectation, sessionID string) bool {
	if !validSessionID(sessionID) {
		return false
	}
	if expected.Required {
		return expected.ID == sessionID
	}
	return expected.ID == ""
}

var _ execution.IdentityObserver = (*identityObserver)(nil)
