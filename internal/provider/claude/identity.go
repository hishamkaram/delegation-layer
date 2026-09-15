package claude

import (
	"bytes"
	"crypto/sha1" // #nosec G505 -- UUIDv5 is defined over SHA-1.
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// FreshSessionID derives the deterministic UUIDv5 used for a new Claude
// session. The decoded root ID is the UUID namespace and the canonical task ID
// is the ASCII name, so preparing the same immutable request is repeatable.
func FreshSessionID(rootID, taskID string) (string, error) {
	if err := task.ValidateRootID(rootID); err != nil {
		return "", err
	}
	if err := task.ValidateTaskID(taskID); err != nil {
		return "", err
	}
	namespace, err := hex.DecodeString(rootID)
	if err != nil || len(namespace) != 16 {
		return "", fmt.Errorf("decoding root UUID namespace: %w", err)
	}
	hasher := sha1.New() // UUIDv5 requires SHA-1 over namespace bytes and name.
	if _, err = hasher.Write(namespace); err != nil {
		return "", fmt.Errorf("hashing session namespace: %w", err)
	}
	if _, err = hasher.Write([]byte(taskID)); err != nil {
		return "", fmt.Errorf("hashing session name: %w", err)
	}
	sum := hasher.Sum(nil)
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(sum[0:4]),
		hex.EncodeToString(sum[4:6]),
		hex.EncodeToString(sum[6:8]),
		hex.EncodeToString(sum[8:10]),
		hex.EncodeToString(sum[10:16]),
	), nil
}

// NewIdentityObserver builds the runner-owned bounded Claude init observer.
// Fresh tasks are bound to the deterministic UUID derived from rootID/taskID;
// continuation tasks are bound to the exact expected session ID.
func NewIdentityObserver(rootID, taskID string, expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
	if err := task.ValidateRootID(rootID); err != nil {
		return nil, err
	}
	if err := task.ValidateTaskID(taskID); err != nil {
		return nil, err
	}
	expectedID, err := expectedSessionID(rootID, taskID, expected)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, errors.New("nil Claude identity recorder")
	}
	observer := &identityObserver{
		expected: expectedID,
		record:   record,
	}
	observer.framer = commonprovider.NewJSONLFramer(maxEventLineBytes, observer.processLine)
	return observer, nil
}

func expectedSessionID(rootID, taskID string, expected task.SessionExpectation) (string, error) {
	if expected.Required {
		if !validSessionID(expected.ID) {
			return "", errors.New("required Claude session ID is invalid")
		}
		return expected.ID, nil
	}
	if expected.ID != "" {
		return "", errors.New("unexpected Claude continuation session ID")
	}
	return FreshSessionID(rootID, taskID)
}

type identityObserver struct {
	mu          sync.Mutex
	expected    string
	record      func(task.SessionIdentity) error
	framer      *commonprovider.JSONLFramer
	callbackErr error
	sessionID   string
	seen        bool
	done        bool
	completeErr error
}

// Observe accepts capture chunks while stdout remains live. It retains only
// one bounded JSONL line and never stops capture after a semantic fault; the
// execution core still needs to drain the native process to EOF.
func (o *identityObserver) Observe(data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return
	}
	o.framer.Feed(data)
}

// Complete flushes a final unterminated line and freezes observation. The
// callback is deliberately allowed before completion so the runner can retain
// the provider reference even when later evidence rejects publication.
func (o *identityObserver) Complete() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return o.completeErr
	}
	o.framer.Finish()
	o.done = true
	// Semantic stream faults belong to the sealed-output predicate. Complete
	// reports only callback/storage faults, matching the observer's narrow role
	// and allowing the runner to retain a reference from an otherwise rejected
	// stream.
	o.completeErr = o.callbackErr
	return o.completeErr
}

func (o *identityObserver) processLine(line []byte) error {
	if len(bytes.TrimSpace(line)) == 0 {
		return errors.New("blank Claude identity event line")
	}
	if !utf8.Valid(line) {
		return errors.New("claude identity event line is not valid UTF-8")
	}
	event, err := decodeEvent(line)
	if err != nil {
		return fmt.Errorf("invalid Claude identity event: %w", err)
	}
	if event.typeName != "system" {
		return nil
	}
	subtype, ok := optionalString(event.fields, "subtype")
	if !ok || subtype != "init" {
		return nil
	}
	if o.seen {
		return errors.New("multiple Claude system/init events")
	}
	sessionID, err := requiredString(event.fields, "session_id")
	if err != nil {
		return fmt.Errorf("claude system/init session_id: %w", err)
	}
	if !validSessionID(sessionID) {
		return errors.New("claude system/init session_id is not a UUID")
	}
	o.seen = true
	o.sessionID = sessionID
	if sessionID == o.expected && o.callbackErr == nil {
		o.callbackErr = o.record(task.SessionIdentity{Provider: Provider, ConversationID: sessionID})
		if o.callbackErr != nil {
			o.callbackErr = fmt.Errorf("recording Claude session identity: %w", o.callbackErr)
		}
	}
	return nil
}

func sessionMatchesExpectation(rootID, taskID string, expected task.SessionExpectation, sessionID string) bool {
	if !validSessionID(sessionID) {
		return false
	}
	expectedID, err := expectedSessionID(rootID, taskID, expected)
	return err == nil && sessionID == expectedID
}

var _ execution.IdentityObserver = (*identityObserver)(nil)
