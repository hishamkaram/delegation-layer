package codex

import (
	"bytes"
	"errors"
	"sync"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// identityObserver buffers one bounded JSONL line and records the first valid,
// expected thread.started identity while capture is live. This observed identity
// is independent of terminal authority: later malformed or conflicting events
// can reject the outcome without erasing the immutable provider reference.
// Complete flushes a final unterminated line and freezes further observation.
type identityObserver struct {
	mu       sync.Mutex
	expected task.SessionExpectation
	record   func(task.SessionIdentity) error
	framer   *commonprovider.JSONLFramer
	threadID string
	seen     bool
	done     bool
	callback error
}

// NewIdentityObserver builds the runner-owned Codex thread identity observer.
// The task ID binds construction to the task that owns the observer; it is not
// sent to Codex and does not influence the observed thread ID.
func NewIdentityObserver(taskID string, expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
	if err := task.ValidateTaskID(taskID); err != nil {
		return nil, err
	}
	if expected.Required && !validThreadID(expected.ID) {
		return nil, errors.New("required codex thread ID is invalid")
	}
	if !expected.Required && expected.ID != "" {
		return nil, errors.New("unexpected codex continuation thread ID")
	}
	if record == nil {
		return nil, errors.New("nil codex identity recorder")
	}
	observer := &identityObserver{expected: expected, record: record}
	observer.framer = commonprovider.NewJSONLFramer(maxEventLineBytes, observer.processLine)
	return observer, nil
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
		return errors.New("blank codex JSONL event line")
	}
	if !utf8.Valid(line) {
		return errors.New("codex identity event line is not valid UTF-8")
	}
	event, err := decodeEvent(line)
	if err != nil {
		return err
	}
	if event.typeName != eventThreadStarted {
		return nil
	}
	threadID, err := requiredString(event.fields, "thread_id")
	if err != nil || !validThreadID(threadID) || o.seen {
		return errors.New("invalid or duplicate thread.started identity")
	}
	o.threadID = threadID
	o.seen = true
	if identityMatchesExpectation(o.expected, threadID) {
		o.callback = o.record(task.SessionIdentity{Provider: Provider, ConversationID: threadID})
	}
	return nil
}

var _ execution.IdentityObserver = (*identityObserver)(nil)

func identityMatchesExpectation(expected task.SessionExpectation, threadID string) bool {
	if !validThreadID(threadID) {
		return false
	}
	if expected.Required {
		return expected.ID == threadID
	}
	return expected.ID == ""
}
