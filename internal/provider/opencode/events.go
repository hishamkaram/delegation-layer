package opencode

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
	eventStepStart  = "step_start"
	eventStepFinish = "step_finish"
	eventText       = "text"
	eventReasoning  = "reasoning"
	eventToolUse    = "tool_use"
	eventError      = "error"
)

type eventState struct {
	sessionID          string
	sessionSeen        bool
	stepStarted        bool
	stepActive         bool
	terminal           bool
	providerFail       bool
	semanticErr        error
	answer             []byte
	stepAnswer         []byte
	stepAnswerSeen     bool
	terminalAnswer     []byte
	terminalAnswerSeen bool
	usage              []*usageTotals
}

type parsedEvent struct {
	typeName string
	fields   map[string]json.RawMessage
}

func newEventState() eventState { return eventState{} }

func (s *eventState) markSemantic(reason string) {
	if s.semanticErr == nil {
		s.semanticErr = fmt.Errorf("invalid OpenCode JSONL evidence: %s", reason)
	}
}

func (s eventState) semanticFault() bool { return s.semanticErr != nil }

// parseStdout drains the complete stream while retaining only bounded parser
// state. Once a semantic fault is found, later bytes are still consumed so the
// execution core can close and seal the provider invocation deterministically.
func parseStdout(reader io.Reader) (eventState, error) {
	state := newEventState()
	semanticErr, readErr := commonprovider.ReadJSONL(reader, maxEventLineBytes, func(line []byte) error {
		return processEventLine(&state, line)
	})
	if state.semanticErr == nil && semanticErr != nil {
		state.markSemantic(semanticErr.Error())
	}
	if readErr != nil {
		return state, fmt.Errorf("reading OpenCode stdout: %w", readErr)
	}
	return state, nil
}

func processEventLine(state *eventState, line []byte) error {
	if state.semanticErr != nil {
		return state.semanticErr
	}
	if !utf8.Valid(line) {
		state.markSemantic("event line is not valid UTF-8")
		return state.semanticErr
	}
	if len(bytes.TrimSpace(line)) == 0 {
		state.markSemantic("blank JSONL event line")
		return state.semanticErr
	}
	event, err := decodeEvent(line)
	if err != nil {
		state.markSemantic(err.Error())
		return state.semanticErr
	}
	applyEvent(state, event)
	return state.semanticErr
}

func decodeEvent(line []byte) (parsedEvent, error) {
	if err := task.ValidateJSONStructure(line); err != nil {
		return parsedEvent{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil || fields == nil {
		return parsedEvent{}, errors.New("event is not a JSON object")
	}
	typeName, err := requiredString(fields, "type")
	if err != nil || typeName == "" {
		return parsedEvent{}, errors.New("event type is not a nonempty string")
	}
	return parsedEvent{typeName: typeName, fields: fields}, nil
}

func applyEvent(state *eventState, event parsedEvent) {
	if state.semanticErr != nil {
		return
	}
	if state.terminal {
		state.markSemantic("event follows terminal step_finish")
		return
	}
	sessionID, err := requiredSessionID(event.fields)
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	if state.sessionSeen && state.sessionID != sessionID {
		state.markSemantic("conflicting sessionID values")
		return
	}
	if !state.sessionSeen {
		state.sessionSeen = true
		state.sessionID = sessionID
	}
	switch event.typeName {
	case eventStepStart:
		applyStepStart(state, event.fields)
	case eventText:
		applyText(state, event.fields)
	case eventReasoning:
		applyReasoning(state, event.fields)
	case eventToolUse:
		applyToolUse(state, event.fields)
	case eventStepFinish:
		applyStepFinish(state, event.fields)
	case eventError:
		applyError(state, event.fields)
	default:
		state.markSemantic("unknown event type: " + event.typeName)
	}
}

func applyStepStart(state *eventState, fields map[string]json.RawMessage) {
	if state.stepActive {
		state.markSemantic("duplicate step_start while a step is active")
		return
	}
	if state.stepStarted && state.terminal {
		state.markSemantic("step_start follows terminal result")
		return
	}
	if err := validatePart(fields, "step-start"); err != nil {
		state.markSemantic(err.Error())
		return
	}
	state.stepStarted = true
	state.stepActive = true
	state.stepAnswer = state.stepAnswer[:0]
	state.stepAnswerSeen = false
}

func applyText(state *eventState, fields map[string]json.RawMessage) {
	if !state.stepActive {
		state.markSemantic("text event is outside an active step")
		return
	}
	part, err := requiredObject(fields, "part")
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	if partType, present := optionalString(part, "type"); present && partType != "text" {
		state.markSemantic("text event part type is not text")
		return
	}
	text, err := requiredString(part, "text")
	if err != nil {
		state.markSemantic("text event part.text is not a string")
		return
	}
	if len(state.answer)+len(text) > maxAnswerBytes {
		state.markSemantic("answer exceeds bounded size")
		return
	}
	state.answer = append(state.answer, text...)
	state.stepAnswer = append(state.stepAnswer, text...)
	state.stepAnswerSeen = true
}

func applyReasoning(state *eventState, fields map[string]json.RawMessage) {
	if !state.stepActive {
		state.markSemantic("reasoning event is outside an active step")
		return
	}
	part, err := requiredObject(fields, "part")
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	if partType, present := optionalString(part, "type"); present && partType != "reasoning" {
		state.markSemantic("reasoning event part type is not reasoning")
		return
	}
	if _, err = requiredString(part, "text"); err != nil {
		state.markSemantic("reasoning event part.text is not a string")
	}
}

func applyToolUse(state *eventState, fields map[string]json.RawMessage) {
	if !state.stepActive {
		state.markSemantic("tool_use event is outside an active step")
		return
	}
	part, err := requiredObject(fields, "part")
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	if partType, present := optionalString(part, "type"); present && partType != "tool" {
		state.markSemantic("tool_use event part type is not tool")
		return
	}
	tool, toolErr := requiredString(part, "tool")
	if toolErr != nil || strings.TrimSpace(tool) == "" {
		state.markSemantic("tool_use event tool is not a nonempty string")
		return
	}
	stateFields, err := requiredObject(part, "state")
	if err != nil {
		state.markSemantic("tool_use event state is not an object")
		return
	}
	status, err := requiredString(stateFields, "status")
	if err != nil {
		state.markSemantic("tool_use event state status is missing or invalid")
		return
	}
	switch status {
	case "completed":
		return
	case "error":
		// A failed tool call is provider evidence, but OpenCode can recover
		// by retrying the step. Validate its bounded diagnostic and let the
		// terminal step determine whether the overall task succeeded.
		if rawError, present := stateFields["error"]; present {
			var diagnostic string
			if err := json.Unmarshal(rawError, &diagnostic); err != nil || !utf8.ValidString(diagnostic) {
				state.markSemantic("tool_use error diagnostic is invalid")
			}
		}
	default:
		state.markSemantic("tool_use event state has unknown status")
	}
}

func applyStepFinish(state *eventState, fields map[string]json.RawMessage) {
	if !state.stepActive {
		state.markSemantic("step_finish is outside an active step")
		return
	}
	part, err := requiredObject(fields, "part")
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	if partType, present := optionalString(part, "type"); present && partType != "step-finish" {
		state.markSemantic("step_finish event part type is not step-finish")
		return
	}
	reason, err := requiredString(part, "reason")
	if err != nil {
		state.markSemantic("step_finish reason is missing or not a string")
		return
	}
	if reason != "stop" && reason != "tool-calls" {
		state.markSemantic("step_finish has unknown reason")
		return
	}
	usage, err := parseUsage(part)
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	if usage != nil {
		appendUsage(state, usage)
	}
	state.stepActive = false
	if reason == "stop" {
		state.terminal = true
		state.terminalAnswer = append(state.terminalAnswer[:0], state.stepAnswer...)
		state.terminalAnswerSeen = state.stepAnswerSeen
	}
}

func appendUsage(state *eventState, usage *usageTotals) {
	if usage == nil {
		return
	}
	if len(state.usage) >= maxUsageRows {
		state.markSemantic(fmt.Sprintf("usage row bound exceeded (%d)", maxUsageRows))
		return
	}
	state.usage = append(state.usage, usage)
}

func applyError(state *eventState, fields map[string]json.RawMessage) {
	errorFields, err := requiredObject(fields, "error")
	if err != nil {
		state.markSemantic("error event error is not an object")
		return
	}
	if name, err := requiredString(errorFields, "name"); err != nil || strings.TrimSpace(name) == "" {
		state.markSemantic("error event name is not a nonempty string")
		return
	}
	state.providerFail = true
}

func validatePart(fields map[string]json.RawMessage, expectedType string) error {
	part, err := requiredObject(fields, "part")
	if err != nil {
		return err
	}
	if partType, present := optionalString(part, "type"); present && partType != expectedType {
		return fmt.Errorf("part type is not %s", expectedType)
	}
	return nil
}

func requiredSessionID(fields map[string]json.RawMessage) (string, error) {
	sessionID, err := requiredString(fields, "sessionID")
	if err != nil {
		return "", errors.New("event sessionID is missing or not a string")
	}
	if !validSessionID(sessionID) {
		return "", errors.New("event sessionID is invalid")
	}
	return sessionID, nil
}

func requiredObject(fields map[string]json.RawMessage, name string) (map[string]json.RawMessage, error) {
	raw, ok := fields[name]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, fmt.Errorf("missing %s object", name)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("%s is not an object", name)
	}
	return object, nil
}

func requiredString(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", fmt.Errorf("missing %s", name)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s is not a string", name)
	}
	return value, nil
}

func optionalString(fields map[string]json.RawMessage, name string) (string, bool) {
	raw, ok := fields[name]
	if !ok {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", true
	}
	return value, true
}
