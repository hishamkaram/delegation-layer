package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
	"unicode"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// currentEventState deliberately retains only lifecycle facts, relevant
// agent-message identities, and offsets for the selected final record. The
// sealed reader is reopened for hashing and output after semantic evaluation.
type currentEventState struct {
	threadID         string
	threadSeen       bool
	turnStarted      bool
	turnCompleted    bool
	terminalFailed   bool
	interrupted      bool
	semantic         error
	final            currentFinal
	usage            *usageTotals
	usageReliable    bool
	requireItemStart bool
	items            map[string]currentItemState
}

type currentFinal struct {
	start    int64
	end      int64
	id       string
	nonblank bool
	seen     bool
}

type currentItemState struct {
	started   bool
	updated   bool
	completed bool
}

func newCurrentEventState(requireItemStart bool) currentEventState {
	return currentEventState{requireItemStart: requireItemStart, items: make(map[string]currentItemState)}
}

func (s *currentEventState) markCurrentSemantic(reason string) {
	if s.semantic == nil {
		s.semantic = fmt.Errorf("invalid current Codex JSONL evidence: %s", reason)
	}
}

func (s *currentEventState) markCurrentFault(err error) {
	if s.semantic == nil {
		s.semantic = err
	}
}

func (s currentEventState) semanticFault() bool { return s.semantic != nil }

func parseCurrentStdout(reader io.Reader, requireItemStart bool) (currentEventState, error) {
	state := newCurrentEventState(requireItemStart)
	semanticErr, readErr := providerjson.ReadRecords(reader, func(value providerjson.Value, _ uint64) error {
		return processCurrentRecord(&state, value)
	})
	if state.semantic == nil && semanticErr != nil {
		state.markCurrentFault(semanticErr)
	}
	if readErr != nil {
		return state, fmt.Errorf("reading current Codex stdout: %w", readErr)
	}
	return state, nil
}

func processCurrentRecord(state *currentEventState, value providerjson.Value) error {
	if state.semantic != nil {
		return state.semantic
	}
	typeFields, err := value.Object("type")
	if err != nil {
		state.markCurrentFault(err)
		return state.semantic
	}
	typeName, err := currentRequiredString(typeFields, "type", maxItemTypeBytes)
	if err != nil {
		state.markCurrentFault(err)
		return state.semantic
	}
	switch typeName {
	case eventThreadStarted:
		return currentThreadStarted(state, value)
	case eventTurnStarted:
		return currentTurnStarted(state)
	case eventTurnCompleted:
		return currentTurnCompleted(state, value)
	case eventTurnFailed:
		return currentTurnFailed(state, value)
	case eventItemStarted, eventItemUpdated, eventItemCompleted:
		return currentItemEvent(state, value, typeName)
	case eventError:
		return currentErrorEvent(state, value)
	case "turn.interrupted", "turn.cancelled", "turn.aborted":
		return currentInterrupted(state)
	default:
		if strings.HasPrefix(typeName, "thread.") || strings.HasPrefix(typeName, "turn.") {
			state.markCurrentSemantic("unknown thread/turn lifecycle event: " + typeName)
		}
	}
	return state.semantic
}

func currentThreadStarted(state *currentEventState, value providerjson.Value) error {
	if state.threadSeen {
		state.markCurrentSemantic("multiple thread.started events")
		return state.semantic
	}
	fields, err := value.Object("thread_id")
	if err != nil {
		state.markCurrentFault(err)
		return state.semantic
	}
	threadID, err := currentRequiredString(fields, "thread_id", maxItemIDBytes)
	if err != nil {
		state.markCurrentSemantic("thread.started thread_id is not a string")
		return state.semantic
	}
	if !validThreadID(threadID) {
		state.markCurrentSemantic("thread.started thread_id is not a UUID")
		return state.semantic
	}
	state.threadSeen = true
	state.threadID = threadID
	return nil
}

func currentTurnStarted(state *currentEventState) error {
	if !state.threadSeen {
		state.markCurrentSemantic("turn.started precedes thread.started")
		return state.semantic
	}
	if state.turnStarted || state.turnCompleted || state.terminalFailed || state.interrupted {
		state.markCurrentSemantic("duplicate or late turn.started event")
		return state.semantic
	}
	state.turnStarted = true
	return nil
}

func currentTurnCompleted(state *currentEventState, value providerjson.Value) error {
	if !state.threadSeen || !state.turnStarted {
		state.markCurrentSemantic("turn.completed has no active turn")
		return state.semantic
	}
	if state.turnCompleted || state.terminalFailed || state.interrupted {
		state.markCurrentSemantic("conflicting turn terminal events")
		return state.semantic
	}
	fields, err := value.Object("status")
	if err != nil {
		state.markCurrentFault(err)
		return state.semantic
	}
	if status, ok := fields["status"]; ok {
		decoded, decodeErr := status.String(maxItemTypeBytes)
		if decodeErr != nil {
			state.markCurrentSemantic("turn.completed status is not a string")
			return state.semantic
		}
		if decoded != "" && decoded != "completed" {
			state.interrupted = true
			return nil
		}
	}
	// Usage is advisory. Validate the whole record above, but isolate the
	// selected usage member so a null, malformed, or duplicate optional usage
	// value becomes unreliable metadata instead of rejecting an otherwise
	// valid terminal answer.
	usageFields, usageErr := value.Object("usage")
	if usageErr != nil {
		state.usage = &usageTotals{present: true}
		state.usageReliable = false
	} else if usage, ok := usageFields["usage"]; ok {
		state.usage, state.usageReliable = parseCurrentUsage(usage)
	}
	state.turnCompleted = true
	return nil
}

func currentTurnFailed(state *currentEventState, value providerjson.Value) error {
	if !state.threadSeen || !state.turnStarted {
		state.markCurrentSemantic("turn.failed has no active turn")
		return state.semantic
	}
	if state.terminalFailed || state.turnCompleted || state.interrupted {
		state.markCurrentSemantic("conflicting turn terminal events")
		return state.semantic
	}
	fields, err := value.Object("error")
	if err != nil {
		state.markCurrentFault(err)
		return state.semantic
	}
	errorValue, ok := fields["error"]
	if !ok {
		state.markCurrentSemantic("turn.failed error is missing")
		return state.semantic
	}
	errorFields, err := errorValue.Object("message")
	if err != nil {
		state.markCurrentSemantic("turn.failed error is not an object")
		return state.semantic
	}
	if _, err = currentRequiredString(errorFields, "message", maxInteractionQuestionText); err != nil {
		state.markCurrentSemantic("turn.failed error message is not a string")
		return state.semantic
	}
	state.terminalFailed = true
	return nil
}

func currentErrorEvent(state *currentEventState, value providerjson.Value) error {
	fields, err := value.Object("message")
	if err != nil {
		state.markCurrentFault(err)
		return state.semantic
	}
	if _, err = currentRequiredString(fields, "message", maxInteractionQuestionText); err != nil {
		state.markCurrentSemantic("error message is not a string")
		return state.semantic
	}
	if state.turnCompleted {
		state.markCurrentSemantic("error event follows turn completion")
	}
	return state.semantic
}

func currentInterrupted(state *currentEventState) error {
	if !state.threadSeen || !state.turnStarted || state.turnCompleted || state.terminalFailed || state.interrupted {
		state.markCurrentSemantic("conflicting turn interruption")
		return state.semantic
	}
	state.interrupted = true
	return nil
}

func currentItemEvent(state *currentEventState, value providerjson.Value, eventType string) error {
	itemValue, typeName, err := currentEventItem(value)
	if err != nil {
		state.markCurrentFault(err)
		return state.semantic
	}
	if state.threadSeen && !state.turnStarted && eventType == eventItemCompleted && typeName == "error" {
		if err := currentErrorItemMessage(itemValue); err != nil {
			state.markCurrentFault(err)
		}
		return state.semantic
	}
	if !state.threadSeen || !state.turnStarted || state.turnCompleted || state.terminalFailed || state.interrupted {
		state.markCurrentSemantic("item event is outside the active turn")
		return state.semantic
	}
	if typeName != itemAgentMessage {
		if typeName == "error" {
			if err := currentErrorItemMessage(itemValue); err != nil {
				state.markCurrentFault(err)
			}
		}
		return state.semantic
	}
	return currentAgentMessageEvent(state, itemValue, value, eventType)
}

func currentEventItem(value providerjson.Value) (providerjson.Value, string, error) {
	fields, err := value.Object("item")
	if err != nil {
		return providerjson.Value{}, "", err
	}
	item, ok := fields["item"]
	if !ok {
		return item, "", errors.New("item event has no item")
	}
	types, err := item.Object("type")
	if err != nil {
		return item, "", errors.New("item event item is not an object")
	}
	name, err := currentRequiredString(types, "type", maxItemTypeBytes)
	if err != nil || name == "" {
		return item, "", errors.New("item type is not a nonempty string")
	}
	return item, name, nil
}

func currentErrorItemMessage(itemValue providerjson.Value) error {
	fields, err := itemValue.Object("type", "message")
	if err != nil {
		return errors.New("error item is not a valid object")
	}
	if _, err = currentRequiredString(fields, "message", maxInteractionQuestionText); err != nil {
		return errors.New("error item message is not a string")
	}
	return nil
}

func currentAgentMessageEvent(state *currentEventState, itemValue providerjson.Value, record providerjson.Value, eventType string) error {
	selected := []string{"type", "id"}
	if eventType == eventItemCompleted {
		selected = append(selected, "text")
	}
	fields, err := itemValue.Object(selected...)
	if err != nil {
		state.markCurrentFault(err)
		return state.semantic
	}
	id, err := currentRequiredString(fields, "id", maxItemIDBytes)
	if err != nil || id == "" {
		state.markCurrentSemantic("agent_message id is not a nonempty string")
		return state.semantic
	}
	if len(id) > maxItemIDBytes {
		state.markCurrentSemantic("agent_message id exceeds bounded size")
		return state.semantic
	}
	itemState, known := state.items[id]
	if !known && len(state.items) >= maxTrackedItems {
		state.markCurrentSemantic("agent_message identity state exceeds bounded count")
		return state.semantic
	}
	if err = transitionCurrentItem(&itemState, eventType, state.requireItemStart); err != nil {
		state.markCurrentFault(err)
		return state.semantic
	}
	state.items[id] = itemState
	if eventType != eventItemCompleted {
		return nil
	}
	textValue, ok := fields["text"]
	if !ok {
		state.markCurrentSemantic("agent_message text is missing")
		return state.semantic
	}
	nonblank, err := textValue.Nonblank()
	if err != nil {
		state.markCurrentSemantic("agent_message text is not a string")
		return state.semantic
	}
	state.final = currentFinal{start: record.Start, end: record.End, id: id, nonblank: nonblank, seen: true}
	return nil
}

func transitionCurrentItem(item *currentItemState, eventType string, requireStart bool) error {
	switch eventType {
	case eventItemStarted:
		if item.started || item.completed {
			return errors.New("duplicate agent_message item.started event")
		}
		item.started = true
	case eventItemUpdated:
		if !item.started {
			return errors.New("agent_message item.updated precedes item.started")
		}
		if item.completed {
			return errors.New("agent_message item.updated follows completion")
		}
		item.updated = true
	case eventItemCompleted:
		if requireStart && !item.started {
			return errors.New("agent_message item.completed precedes item.started")
		}
		if item.completed {
			return errors.New("duplicate agent_message item.completed event")
		}
		item.completed = true
	default:
		return errors.New("unknown agent_message item event")
	}
	return nil
}

func currentRequiredString(fields map[string]providerjson.Value, name string, limit int) (string, error) {
	value, ok := fields[name]
	if !ok {
		return "", fmt.Errorf("missing %s", name)
	}
	decoded, err := value.String(limit)
	if err != nil {
		return "", fmt.Errorf("%s is not a bounded string", name)
	}
	return decoded, nil
}

func currentRefusalFor(input predicate.Input, state currentEventState, provider string) string {
	if refusal := sealRefusal(input.Seal); refusal != "" {
		return refusal
	}
	if state.semanticFault() {
		return refusalMalformed
	}
	if state.terminalFailed || state.interrupted {
		return refusalProviderFailed
	}
	if !state.threadSeen || !state.turnStarted {
		return refusalMalformed
	}
	if !identityMatchesForProvider(input, state.threadID, provider) {
		return refusalIdentity
	}
	if !state.turnCompleted {
		return refusalIncomplete
	}
	if !state.final.seen || !state.final.nonblank {
		return refusalEmpty
	}
	return ""
}

func currentUsageMetadata(state currentEventState, reliability string) []task.UsageMetadata {
	if state.usage == nil || !state.usage.present {
		return nil
	}
	if !state.usageReliable {
		reliability = task.UsageReliabilityUnreliable
	}
	return usageFromLastTotal(state.usage, reliability)
}

func parseCurrentUsage(value providerjson.Value) (*usageTotals, bool) {
	usage := &usageTotals{present: true}
	fields, err := value.Object("input_tokens", "cached_input_tokens", "cache_write_input_tokens", "output_tokens", "reasoning_output_tokens")
	if err != nil {
		return usage, false
	}
	reliable := true
	known := 0
	usage.input, reliable = currentUsageCounter(fields, "input_tokens", reliable, &known)
	usage.cachedInput, reliable = currentUsageCounter(fields, "cached_input_tokens", reliable, &known)
	usage.cacheWrite, reliable = currentUsageCounter(fields, "cache_write_input_tokens", reliable, &known)
	usage.output, reliable = currentUsageCounter(fields, "output_tokens", reliable, &known)
	usage.reasoning, reliable = currentUsageCounter(fields, "reasoning_output_tokens", reliable, &known)
	if known == 0 {
		reliable = false
	}
	return usage, reliable
}

func currentUsageCounter(fields map[string]providerjson.Value, name string, reliable bool, known *int) (*int64, bool) {
	value, ok := fields[name]
	if !ok {
		return nil, reliable
	}
	(*known)++
	raw, err := value.Raw(64)
	if err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false
	}
	var decoded int64
	if err = json.Unmarshal(raw, &decoded); err != nil || decoded < 0 {
		return nil, false
	}
	return &decoded, reliable
}

// currentDigest is a streaming length/hash/nonblank summary of a decoded
// answer. It never retains answer bytes.
type currentDigest struct {
	hash     hash.Hash
	length   int64
	nonblank bool
}

func newCurrentDigest() *currentDigest { return &currentDigest{hash: sha256.New()} }

func (d *currentDigest) Write(data []byte) (int, error) {
	n, err := d.hash.Write(data)
	if err != nil {
		return n, err
	}
	if n != len(data) {
		return n, io.ErrShortWrite
	}
	d.length += int64(n)
	for _, r := range string(data) {
		if !unicode.IsSpace(r) {
			d.nonblank = true
			break
		}
	}
	return n, nil
}

func (d *currentDigest) sum() []byte { return d.hash.Sum(nil) }

type currentAnswerWriteError struct{ err error }

func (e *currentAnswerWriteError) Error() string { return e.err.Error() }
func (e *currentAnswerWriteError) Unwrap() error { return e.err }

type currentOutputWriter struct {
	digest *currentDigest
	out    io.Writer
}

func (w *currentOutputWriter) Write(data []byte) (int, error) {
	if w.out == nil {
		return 0, &currentAnswerWriteError{err: errors.New("nil Codex answer sink")}
	}
	n, err := w.out.Write(data)
	if n < 0 || n > len(data) {
		return n, &currentAnswerWriteError{err: fmt.Errorf("codex answer sink returned invalid byte count %d", n)}
	}
	if err != nil {
		return n, &currentAnswerWriteError{err: err}
	}
	if n != len(data) {
		return n, &currentAnswerWriteError{err: io.ErrShortWrite}
	}
	return w.digest.Write(data)
}

func readCurrentArtifact(raw predicate.Evidence) (currentDigest, bool, error) {
	named, ok := raw.(predicate.NamedEvidence)
	if !ok {
		return currentDigest{}, false, errors.New("codex predicate requires named evidence")
	}
	digest := newCurrentDigest()
	err := named.ReadNamed(OutputName, func(reader io.Reader) error {
		_, copyErr := io.Copy(digest, reader)
		return copyErr
	})
	if errors.Is(err, predicate.ErrEvidenceAbsent) {
		return currentDigest{}, false, nil
	}
	if err != nil {
		return currentDigest{}, false, err
	}
	return *digest, true, nil
}

func currentFinalText(value providerjson.Value) (providerjson.Value, error) {
	typeFields, err := value.Object("type")
	if err != nil {
		return providerjson.Value{}, err
	}
	typeName, err := currentRequiredString(typeFields, "type", maxItemTypeBytes)
	if err != nil || typeName != eventItemCompleted {
		return providerjson.Value{}, errors.New("selected Codex record is not item.completed")
	}
	topFields, err := value.Object("item")
	if err != nil {
		return providerjson.Value{}, err
	}
	itemValue, ok := topFields["item"]
	if !ok {
		return providerjson.Value{}, errors.New("selected Codex record has no item")
	}
	itemFields, err := itemValue.Object("type", "id", "text")
	if err != nil {
		return providerjson.Value{}, err
	}
	typeName, err = currentRequiredString(itemFields, "type", maxItemTypeBytes)
	if err != nil || typeName != itemAgentMessage {
		return providerjson.Value{}, errors.New("selected Codex record is not an agent message")
	}
	if _, err = currentRequiredString(itemFields, "id", maxItemIDBytes); err != nil {
		return providerjson.Value{}, err
	}
	textValue, ok := itemFields["text"]
	if !ok {
		return providerjson.Value{}, errors.New("selected agent message has no text")
	}
	return textValue, nil
}

func streamCurrentFinal(raw predicate.Evidence, final currentFinal, out io.Writer) (currentDigest, error) {
	digest := newCurrentDigest()
	writer := &currentOutputWriter{digest: digest, out: out}
	var found bool
	var semanticErr, readErr error
	readErr = raw.Read(predicate.Stdout, func(reader io.Reader) error {
		semanticErr, readErr = providerjson.ReadRecords(reader, func(value providerjson.Value, _ uint64) error {
			if value.Start != final.start || value.End != final.end {
				return nil
			}
			found = true
			textValue, err := currentFinalText(value)
			if err != nil {
				return err
			}
			return textValue.CopyString(writer)
		})
		return readErr
	})
	if readErr != nil {
		return currentDigest{}, readErr
	}
	if semanticErr != nil {
		var writeErr *currentAnswerWriteError
		if errors.As(semanticErr, &writeErr) {
			return currentDigest{}, writeErr.err
		}
		return currentDigest{}, fmt.Errorf("streaming current Codex answer: %w", semanticErr)
	}
	if !found {
		return currentDigest{}, errors.New("selected Codex answer record was not found")
	}
	return *digest, nil
}

func digestsEqual(left, right currentDigest) bool {
	return left.length == right.length && bytes.Equal(left.sum(), right.sum())
}
