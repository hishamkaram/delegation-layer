package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	eventThreadStarted = "thread.started"
	eventTurnStarted   = "turn.started"
	eventTurnCompleted = "turn.completed"
	eventTurnFailed    = "turn.failed"
	eventItemStarted   = "item.started"
	eventItemUpdated   = "item.updated"
	eventItemCompleted = "item.completed"
	eventError         = "error"

	itemAgentMessage = "agent_message"
)

type eventState struct {
	threadID       string
	threadSeen     bool
	turnStarted    bool
	turnCompleted  bool
	terminalFailed bool
	interrupted    bool
	semantic       error
	finalMessage   []byte
	finalSeen      bool
	usage          *usageTotals
	items          map[string]itemState
}

type itemState struct {
	typeName  string
	started   bool
	updated   bool
	completed bool
}

// parsedEvent is intentionally small. Unknown event objects are discarded
// after their structure and duplicate keys have been checked.
type parsedEvent struct {
	typeName string
	fields   map[string]json.RawMessage
}

func newEventState() eventState {
	return eventState{items: make(map[string]itemState)}
}

func (s *eventState) markSemantic(reason string) {
	if s.semantic == nil {
		s.semantic = fmt.Errorf("invalid codex JSONL evidence: %s", reason)
	}
}

func (s eventState) semanticFault() bool { return s.semantic != nil }

// parseStdout consumes all available bytes, retaining at most one bounded
// event line and the final answer candidate. Semantic faults remain recorded
// while the stream is drained so the core can safely close and seal capture.
func parseStdout(reader io.Reader) (eventState, error) {
	state := newEventState()
	semanticErr, readErr := commonprovider.ReadJSONL(reader, maxEventLineBytes, func(line []byte) error {
		return processEventLine(&state, line)
	})
	if state.semantic == nil && semanticErr != nil {
		if errors.Is(semanticErr, commonprovider.ErrJSONLLineTooLong) {
			state.markSemantic("event line exceeds 1 MiB")
		} else {
			state.markSemantic(semanticErr.Error())
		}
	}
	if readErr != nil {
		return state, fmt.Errorf("reading codex stdout: %w", readErr)
	}
	return state, nil
}

func processEventLine(state *eventState, line []byte) error {
	// Once a permanent semantic fault is known, no later event can restore a
	// publishable result. Keep consuming bytes for real reader faults, but do
	// not parse or retain any more event state.
	if state.semantic != nil {
		return state.semantic
	}
	if len(bytes.TrimSpace(line)) == 0 {
		state.markSemantic("blank JSONL event line")
		return state.semantic
	}
	if !utf8.Valid(line) {
		state.markSemantic("event line is not valid UTF-8")
		return state.semantic
	}
	event, err := decodeEvent(line)
	if err != nil {
		state.markSemantic(err.Error())
		return state.semantic
	}
	applyEvent(state, event)
	return state.semantic
}

func decodeEvent(line []byte) (parsedEvent, error) {
	if err := task.ValidateJSONStructure(line); err != nil {
		return parsedEvent{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return parsedEvent{}, fmt.Errorf("decoding event object: %w", err)
	}
	if fields == nil {
		return parsedEvent{}, errors.New("event is not a JSON object")
	}
	rawType, ok := fields["type"]
	if !ok {
		return parsedEvent{}, errors.New("event type is missing")
	}
	var typeName string
	if err := json.Unmarshal(rawType, &typeName); err != nil || typeName == "" {
		return parsedEvent{}, errors.New("event type is not a nonempty string")
	}
	return parsedEvent{typeName: typeName, fields: fields}, nil
}

func applyEvent(state *eventState, event parsedEvent) {
	if state.semantic != nil {
		return
	}
	switch event.typeName {
	case eventThreadStarted:
		applyThreadStarted(state, event.fields)
	case eventTurnStarted:
		applyTurnStarted(state)
	case eventTurnCompleted:
		applyTurnCompleted(state, event.fields)
	case eventTurnFailed:
		applyTurnFailed(state, event.fields)
	case eventItemStarted, eventItemUpdated, eventItemCompleted:
		applyItemEvent(state, event.typeName, event.fields)
	case eventError:
		applyErrorEvent(state, event.fields)
	case "turn.interrupted", "turn.cancelled", "turn.aborted":
		applyInterrupted(state)
	default:
		// Ordinary telemetry and item extensions are tolerated. A new lifecycle
		// name could carry a terminal state, so preserve ambiguity as a semantic
		// rejection instead of silently accepting a successful turn.
		if strings.HasPrefix(event.typeName, "thread.") || strings.HasPrefix(event.typeName, "turn.") {
			state.markSemantic("unknown thread/turn lifecycle event: " + event.typeName)
		}
	}
}

func applyThreadStarted(state *eventState, fields map[string]json.RawMessage) {
	if state.threadSeen {
		state.markSemantic("multiple thread.started events")
		return
	}
	threadID, err := requiredString(fields, "thread_id")
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	if !validThreadID(threadID) {
		state.markSemantic("thread.started thread_id is not a UUID")
		return
	}
	state.threadSeen = true
	state.threadID = threadID
}

func applyTurnStarted(state *eventState) {
	if !state.threadSeen {
		state.markSemantic("turn.started precedes thread.started")
		return
	}
	if state.turnStarted || state.turnCompleted || state.terminalFailed || state.interrupted {
		state.markSemantic("duplicate or late turn.started event")
		return
	}
	state.turnStarted = true
}

func applyTurnCompleted(state *eventState, fields map[string]json.RawMessage) {
	if !state.threadSeen || !state.turnStarted {
		state.markSemantic("turn.completed has no active turn")
		return
	}
	if state.turnCompleted || state.terminalFailed || state.interrupted {
		state.markSemantic("conflicting turn terminal events")
		return
	}
	if rawStatus, ok := fields["status"]; ok {
		status, err := decodeString(rawStatus)
		if err != nil {
			state.markSemantic("turn.completed status is not a string")
			return
		}
		if status != "" && status != "completed" {
			state.interrupted = true
			return
		}
	}
	usage, err := parseUsage(fields)
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	state.turnCompleted = true
	state.usage = usage
}

func applyTurnFailed(state *eventState, fields map[string]json.RawMessage) {
	if !state.threadSeen || !state.turnStarted {
		state.markSemantic("turn.failed has no active turn")
		return
	}
	if state.terminalFailed || state.turnCompleted || state.interrupted {
		state.markSemantic("conflicting turn terminal events")
		return
	}
	if rawError, ok := fields["error"]; ok {
		var errorFields map[string]json.RawMessage
		if err := json.Unmarshal(rawError, &errorFields); err != nil || errorFields == nil {
			state.markSemantic("turn.failed error is not an object")
			return
		}
		if _, err := requiredString(errorFields, "message"); err != nil {
			state.markSemantic("turn.failed error message is not a string")
			return
		}
	} else {
		state.markSemantic("turn.failed error is missing")
		return
	}
	state.terminalFailed = true
}

func applyErrorEvent(state *eventState, fields map[string]json.RawMessage) {
	if _, err := requiredString(fields, "message"); err != nil {
		state.markSemantic("error message is not a string")
		return
	}
	if state.turnCompleted {
		state.markSemantic("error event follows turn completion")
	}
}

func applyInterrupted(state *eventState) {
	if !state.threadSeen || !state.turnStarted || state.turnCompleted || state.terminalFailed || state.interrupted {
		state.markSemantic("conflicting turn interruption")
		return
	}
	state.interrupted = true
}

func applyItemEvent(state *eventState, eventType string, fields map[string]json.RawMessage) {
	if !state.threadSeen || !state.turnStarted || state.turnCompleted || state.terminalFailed || state.interrupted {
		state.markSemantic("item event is outside the active turn")
		return
	}
	eventItem, err := decodeItemEvent(fields)
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	itemValue, knownItem := state.items[eventItem.id]
	if !knownItem && len(state.items) >= maxTrackedItems {
		state.markSemantic("item identity state exceeds bounded count")
		return
	}
	if itemValue.typeName != "" && itemValue.typeName != eventItem.typeName {
		state.markSemantic("item id changed type")
		return
	}
	itemValue.typeName = eventItem.typeName
	if err = transitionItem(&itemValue, eventType); err != nil {
		state.markSemantic(err.Error())
		return
	}
	if eventType == eventItemCompleted && eventItem.typeName == itemAgentMessage {
		// Only the candidate selected by the native processor is retained;
		// started and updated item text is intentionally discarded.
		state.finalMessage = []byte(eventItem.text)
		state.finalSeen = true
	}
	state.items[eventItem.id] = itemValue
}

type itemEvent struct {
	id       string
	typeName string
	text     string
}

func decodeItemEvent(fields map[string]json.RawMessage) (itemEvent, error) {
	rawItem, ok := fields["item"]
	if !ok {
		return itemEvent{}, errors.New("item event has no item")
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(rawItem, &item); err != nil || item == nil {
		return itemEvent{}, errors.New("item event item is not an object")
	}
	itemID, err := requiredString(item, "id")
	if err != nil || itemID == "" {
		return itemEvent{}, errors.New("item id is not a nonempty string")
	}
	if len(itemID) > maxItemIDBytes {
		return itemEvent{}, errors.New("item id exceeds bounded size")
	}
	typeName, err := requiredString(item, "type")
	if err != nil || typeName == "" {
		return itemEvent{}, errors.New("item type is not a nonempty string")
	}
	if len(typeName) > maxItemTypeBytes {
		return itemEvent{}, errors.New("item type exceeds bounded size")
	}
	text := ""
	if typeName == itemAgentMessage {
		var textErr error
		text, textErr = requiredString(item, "text")
		if textErr != nil {
			return itemEvent{}, errors.New("agent_message text is not a string")
		}
	}
	return itemEvent{id: itemID, typeName: typeName, text: text}, nil
}

func transitionItem(item *itemState, eventType string) error {
	switch eventType {
	case eventItemStarted:
		if item.started || item.completed {
			return errors.New("duplicate item.started event")
		}
		item.started = true
	case eventItemUpdated:
		if item.completed {
			return errors.New("item.updated follows completion")
		}
		item.updated = true
	case eventItemCompleted:
		if item.completed {
			return errors.New("duplicate item.completed event")
		}
		item.completed = true
	default:
		return errors.New("unknown item event")
	}
	return nil
}

func requiredString(fields map[string]json.RawMessage, name string) (string, error) {
	value, ok := fields[name]
	if !ok {
		return "", fmt.Errorf("missing %s", name)
	}
	decoded, err := decodeString(value)
	if err != nil {
		return "", fmt.Errorf("%s is not a string", name)
	}
	return decoded, nil
}

func decodeString(raw json.RawMessage) (string, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", errors.New("null is not a string")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}
