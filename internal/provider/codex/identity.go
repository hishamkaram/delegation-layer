package codex

import (
	"bytes"
	"errors"
	"sync"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/execution"
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
	line     []byte
	tooBig   bool
	semantic bool
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
	return &identityObserver{expected: expected, record: record}, nil
}

func (o *identityObserver) Observe(data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return
	}
	for _, byteValue := range data {
		if o.semantic {
			return
		}
		if byteValue == '\n' {
			o.finishLine(true)
			continue
		}
		if o.tooBig {
			continue
		}
		if len(o.line) >= maxEventLineBytes {
			o.tooBig = true
			o.semantic = true
			continue
		}
		o.line = append(o.line, byteValue)
	}
}

func (o *identityObserver) Complete() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return o.callback
	}
	if o.tooBig || len(o.line) != 0 {
		o.finishLine(false)
	}
	o.line = nil
	o.done = true
	return o.callback
}

func (o *identityObserver) finishLine(blankLineIsFault bool) {
	if o.tooBig {
		o.tooBig = false
		o.line = o.line[:0]
		return
	}
	if len(o.line) == 0 {
		if blankLineIsFault {
			o.semantic = true
		}
		return
	}
	line := o.line
	o.line = o.line[:0]
	if len(bytes.TrimSpace(line)) == 0 {
		o.semantic = true
		return
	}
	if !validIdentityLine(line) {
		o.semantic = true
		return
	}
	event, err := decodeEvent(line)
	if err != nil {
		o.semantic = true
		return
	}
	if event.typeName != eventThreadStarted {
		return
	}
	threadID, err := requiredString(event.fields, "thread_id")
	if err != nil || !validThreadID(threadID) || o.seen {
		o.semantic = true
		return
	}
	o.threadID = threadID
	o.seen = true
	if identityMatchesExpectation(o.expected, threadID) {
		o.callback = o.record(task.SessionIdentity{Provider: Provider, ConversationID: threadID})
	}
}

func validIdentityLine(line []byte) bool {
	return len(line) <= maxEventLineBytes && utf8.Valid(line)
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
