package pi

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
	eventSession        = "session"
	eventAgentStart     = "agent_start"
	eventAgentEnd       = "agent_end"
	eventTurnStart      = "turn_start"
	eventTurnEnd        = "turn_end"
	eventMessageStart   = "message_start"
	eventMessageUpdate  = "message_update"
	eventMessageEnd     = "message_end"
	eventAutoRetryStart = "auto_retry_start"
	eventAutoRetryEnd   = "auto_retry_end"
	eventAgentSettled   = "agent_settled"
	eventError          = "error"
)

type parserOptions struct {
	allowNativeRetry          bool
	settledTerminal           bool
	rejectTrailingRetryEnd    bool
	requireAgentEndErrorMatch bool
}

type eventState struct {
	sessionID          string
	sessionSeen        bool
	agentStarted       bool
	agentEnded         bool
	turnStarted        bool
	turnEnded          bool
	turnStopReason     string
	turnErrorMessage   string
	messageOpen        bool
	providerFail       bool
	stickyProviderFail bool
	semanticErr        error

	allowNativeRetry          bool
	settledTerminal           bool
	rejectTrailingRetryEnd    bool
	requireAgentEndErrorMatch bool
	retryPending              bool

	lastMessageText       []byte
	lastMessageSeen       bool
	lastMessageStopReason string
	messageUsage          *usageTotals
	turnText              []byte
	agentText             []byte
	agentSettled          bool
	usage                 []*usageTotals
}

type parsedEvent struct {
	typeName string
	fields   map[string]json.RawMessage
}

func (s *eventState) markSemantic(reason string) {
	if s.semanticErr == nil {
		s.semanticErr = fmt.Errorf("invalid Pi JSONL evidence: %s", reason)
	}
}

func (s eventState) semanticFault() bool { return s.semanticErr != nil }

// parseStdout drains the complete bounded stream. A semantic fault freezes
// parser state but never stops reads, allowing the execution core to close
// pipes and seal the invocation deterministically.
func parseStdout(reader io.Reader, options ...parserOptions) (eventState, error) {
	parserOptionsValue := parserOptions{}
	if len(options) > 0 {
		parserOptionsValue = options[0]
	}
	state := eventState{
		allowNativeRetry:          parserOptionsValue.allowNativeRetry,
		settledTerminal:           parserOptionsValue.settledTerminal,
		rejectTrailingRetryEnd:    parserOptionsValue.rejectTrailingRetryEnd,
		requireAgentEndErrorMatch: parserOptionsValue.requireAgentEndErrorMatch,
	}
	semanticErr, readErr := commonprovider.ReadJSONL(reader, maxEventLineBytes, func(line []byte) error {
		return processEventLine(&state, line)
	})
	if state.semanticErr == nil && semanticErr != nil {
		state.markSemantic(semanticErr.Error())
	}
	if readErr != nil {
		return state, fmt.Errorf("reading Pi stdout: %w", readErr)
	}
	return state, nil
}

func processEventLine(state *eventState, line []byte) error {
	if state.semanticErr != nil {
		return state.semanticErr
	}
	if len(bytes.TrimSpace(line)) == 0 {
		state.markSemantic("blank JSONL event line")
		return state.semanticErr
	}
	if !utf8.Valid(line) {
		state.markSemantic("event line is not valid UTF-8")
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
	if err := json.Unmarshal(line, &fields); err != nil {
		return parsedEvent{}, fmt.Errorf("decoding Pi event object: %w", err)
	}
	if fields == nil {
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
	if state.agentSettled && state.settledTerminal {
		state.markSemantic("event appears after agent_settled")
		return
	}
	if state.agentEnded {
		if state.allowNativeRetry && event.typeName == eventAgentStart {
			beginAgentCycle(state)
			applyAgentStart(state)
			return
		}
		applyAfterAgentEnd(state, event)
		return
	}
	applyActiveEvent(state, event)
}

func applyActiveEvent(state *eventState, event parsedEvent) {
	switch event.typeName {
	case eventSession:
		applySessionEvent(state, event.fields)
	case eventAgentStart:
		applyAgentStart(state)
	case eventTurnStart:
		applyTurnStart(state)
	case eventMessageStart:
		applyMessageStart(state, event.fields)
	case eventMessageUpdate:
		applyMessageUpdate(state, event.fields)
	case eventMessageEnd:
		applyMessageEnd(state, event.fields)
	case eventTurnEnd:
		applyTurnEnd(state, event.fields)
	case eventAgentEnd:
		applyAgentEnd(state, event.fields)
	case eventAutoRetryStart:
		if state.allowNativeRetry {
			applyNativeRetryStart(state)
		} else {
			applyUnknownEvent(state, event.typeName)
		}
	case eventAutoRetryEnd:
		applyUnknownEvent(state, event.typeName)
	case eventError:
		state.providerFail = true
		state.stickyProviderFail = true
	default:
		applyUnknownEvent(state, event.typeName)
	}
}

func applyAfterAgentEnd(state *eventState, event parsedEvent) {
	switch event.typeName {
	case eventAgentEnd:
		state.markSemantic("duplicate agent_end event")
	case eventAgentSettled:
		if state.agentSettled {
			state.markSemantic("duplicate agent_settled event")
			return
		}
		state.agentSettled = true
	case eventAutoRetryStart:
		if state.allowNativeRetry {
			applyNativeRetryStart(state)
		} else {
			applyUnknownEvent(state, event.typeName)
		}
	case eventAutoRetryEnd:
		if state.rejectTrailingRetryEnd && state.allowNativeRetry && !state.providerFail && !state.retryPending {
			state.markSemantic("auto_retry_end appears after agent_end")
		} else {
			applyUnknownEvent(state, event.typeName)
		}
	case eventSession, eventAgentStart, eventTurnStart, eventTurnEnd, eventMessageStart, eventMessageUpdate, eventMessageEnd:
		state.markSemantic("event appears after agent_end: " + event.typeName)
	case eventError:
		state.providerFail = true
		state.stickyProviderFail = true
	default:
		applyUnknownEvent(state, event.typeName)
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

func applyAgentStart(state *eventState) {
	if !state.sessionSeen || state.agentStarted || state.turnStarted {
		state.markSemantic("invalid agent_start lifecycle")
		return
	}
	state.agentStarted = true
}

func applyNativeRetryStart(state *eventState) {
	if !state.allowNativeRetry || !state.agentEnded || state.retryPending {
		state.markSemantic("invalid auto_retry_start lifecycle")
		return
	}
	state.retryPending = true
}

func beginAgentCycle(state *eventState) {
	state.agentStarted = false
	state.agentEnded = false
	state.turnStarted = false
	state.turnEnded = false
	state.turnStopReason = ""
	state.turnErrorMessage = ""
	state.messageOpen = false
	state.messageUsage = nil
	state.lastMessageText = state.lastMessageText[:0]
	state.lastMessageSeen = false
	state.lastMessageStopReason = ""
	state.turnText = state.turnText[:0]
	state.agentText = state.agentText[:0]
	state.retryPending = false
	state.providerFail = state.stickyProviderFail
}

func applyTurnStart(state *eventState) {
	if !state.agentStarted || (state.turnStarted && !state.turnEnded) {
		state.markSemantic("invalid turn_start lifecycle")
		return
	}
	// A Pi agent may complete a tool-calling turn and then start another turn
	// for the assistant's final response. Reset only per-turn evidence; the
	// session, completed usage rows, and final agent message remain shared.
	if state.turnEnded {
		state.turnEnded = false
		state.turnStopReason = ""
		state.turnErrorMessage = ""
		state.lastMessageText = state.lastMessageText[:0]
		state.lastMessageSeen = false
		state.lastMessageStopReason = ""
		state.turnText = state.turnText[:0]
	}
	state.turnStarted = true
}

func applyMessageStart(state *eventState, fields map[string]json.RawMessage) {
	if !state.turnStarted || state.turnEnded || state.messageOpen {
		state.markSemantic("invalid message_start lifecycle")
		return
	}
	raw, ok := fields["message"]
	if !ok {
		state.markSemantic("message_start message is missing")
		return
	}
	if _, err := decodeMessage(raw); err != nil {
		state.markSemantic("message_start message: " + err.Error())
		return
	}
	state.messageUsage = nil
	state.messageOpen = true
}

func applyMessageUpdate(state *eventState, fields map[string]json.RawMessage) {
	if !state.turnStarted || state.turnEnded {
		state.markSemantic("invalid message_update lifecycle")
		return
	}
	usage, err := validateMessageUpdate(fields)
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	if usage != nil {
		state.messageUsage = usage
	}
}

func applyMessageEnd(state *eventState, fields map[string]json.RawMessage) {
	if !state.turnStarted || state.turnEnded || !state.messageOpen {
		state.markSemantic("invalid message_end lifecycle")
		return
	}
	raw, ok := fields["message"]
	if !ok {
		state.markSemantic("message_end message is missing")
		return
	}
	message, err := decodeMessage(raw)
	if err != nil {
		state.markSemantic("message_end message: " + err.Error())
		return
	}
	state.messageOpen = false
	if message.role == "assistant" {
		if message.errorMessage != "" {
			state.providerFail = true
		}
		if err = validateCompletedAssistantStopReason(message.stopReason); err != nil {
			state.markSemantic("message_end assistant stop reason: " + err.Error())
			return
		}
		if failedAssistantStopReason(message.stopReason) {
			state.providerFail = true
		}
		if message.usage != nil {
			appendUsage(state, message.usage)
		} else if state.messageUsage != nil {
			appendUsage(state, state.messageUsage)
		}
		state.messageUsage = nil
		state.lastMessageText = append(state.lastMessageText[:0], message.text...)
		state.lastMessageSeen = true
		state.lastMessageStopReason = message.stopReason
	}
}

func applyTurnEnd(state *eventState, fields map[string]json.RawMessage) {
	if !state.turnStarted || state.turnEnded || state.messageOpen {
		state.markSemantic("invalid turn_end lifecycle")
		return
	}
	raw, ok := fields["message"]
	if !ok {
		state.markSemantic("turn_end message is missing")
		return
	}
	message, err := decodeMessage(raw)
	if err != nil {
		state.markSemantic("turn_end message: " + err.Error())
		return
	}
	if err = validateToolResults(fields); err != nil {
		state.markSemantic(err.Error())
		return
	}
	if message.role != "assistant" {
		state.markSemantic("turn_end message is not an assistant message")
		return
	}
	if message.errorMessage != "" {
		state.providerFail = true
	}
	if err = validateCompletedAssistantStopReason(message.stopReason); err != nil {
		state.markSemantic("turn_end assistant stop reason: " + err.Error())
		return
	}
	if failedAssistantStopReason(message.stopReason) {
		state.providerFail = true
	}
	if state.lastMessageSeen && !bytes.Equal(state.lastMessageText, message.text) {
		state.markSemantic("turn_end assistant text conflicts with message_end")
		return
	}
	if state.lastMessageStopReason != "" && state.lastMessageStopReason != message.stopReason {
		state.markSemantic("turn_end assistant stop reason conflicts with message_end")
		return
	}
	state.turnText = append(state.turnText[:0], message.text...)
	state.turnStopReason = message.stopReason
	state.turnErrorMessage = message.errorMessage
	state.turnEnded = true
}

func applyAgentEnd(state *eventState, fields map[string]json.RawMessage) {
	if !state.turnEnded || state.agentEnded {
		state.markSemantic("invalid agent_end lifecycle")
		return
	}
	raw, ok := fields["messages"]
	if !ok {
		state.markSemantic("agent_end messages are missing")
		return
	}
	message, err := decodeFinalAgentMessage(raw)
	if err != nil {
		state.markSemantic("agent_end messages: " + err.Error())
		return
	}
	if !state.allowNativeRetry {
		applyLegacyAgentEnd(state, message)
		return
	}
	if message.errorMessage != "" {
		state.providerFail = true
	}
	if state.turnStopReason != message.stopReason {
		state.markSemantic("agent_end assistant stop reason conflicts with turn_end")
		return
	}
	if !bytes.Equal(state.turnText, message.text) {
		state.markSemantic("agent_end assistant text conflicts with turn_end")
		return
	}
	if state.requireAgentEndErrorMatch && state.turnErrorMessage != message.errorMessage {
		state.markSemantic("agent_end assistant error message conflicts with turn_end")
		return
	}
	if message.stopReason != "stop" {
		if failedAssistantStopReason(message.stopReason) {
			state.providerFail = true
			state.agentEnded = true
			return
		}
		state.markSemantic("agent_end final assistant stop reason is not stop")
		return
	}
	state.agentText = append(state.agentText[:0], message.text...)
	state.agentEnded = true
}

func applyLegacyAgentEnd(state *eventState, message messageValue) {
	if message.errorMessage != "" {
		state.providerFail = true
	}
	if message.stopReason != "stop" {
		if failedAssistantStopReason(message.stopReason) {
			state.providerFail = true
			return
		}
		state.markSemantic("agent_end final assistant stop reason is not stop")
		return
	}
	if state.turnStopReason != "stop" {
		state.markSemantic("agent_end follows a nonterminal final turn")
		return
	}
	if !bytes.Equal(state.turnText, message.text) {
		state.markSemantic("agent_end assistant text conflicts with turn_end")
		return
	}
	state.agentText = append(state.agentText[:0], message.text...)
	state.agentEnded = true
}

func applyUnknownEvent(state *eventState, eventType string) {
	if strings.HasPrefix(eventType, "session_") || strings.HasPrefix(eventType, "agent_") || strings.HasPrefix(eventType, "turn_") {
		state.markSemantic("unknown lifecycle event: " + eventType)
	}
}

func applySessionEvent(state *eventState, fields map[string]json.RawMessage) {
	if state.sessionSeen {
		state.markSemantic("multiple session events")
		return
	}
	if state.agentStarted || state.turnStarted {
		state.markSemantic("session event appears after agent start")
		return
	}
	id, err := requiredString(fields, "id")
	if err != nil || !validSessionID(id) {
		state.markSemantic("session id is invalid")
		return
	}
	if rawVersion, ok := fields["version"]; ok {
		var version int64
		if err := json.Unmarshal(rawVersion, &version); err != nil || version <= 0 {
			state.markSemantic("session version is invalid")
			return
		}
	}
	if rawCWD, ok := fields["cwd"]; ok {
		cwd, err := decodeString(rawCWD)
		if err != nil || cwd == "" || !utf8.ValidString(cwd) {
			state.markSemantic("session cwd is invalid")
			return
		}
	}
	state.sessionID = id
	state.sessionSeen = true
}

type messageValue struct {
	role         string
	text         []byte
	stopReason   string
	errorMessage string
	usage        *usageTotals
}

func decodeMessage(raw json.RawMessage) (messageValue, error) {
	if err := task.ValidateJSONStructure(raw); err != nil {
		return messageValue{}, err
	}
	fields, err := decodeObject(raw)
	if err != nil {
		return messageValue{}, errors.New("message is not an object")
	}
	role, err := requiredString(fields, "role")
	if err != nil {
		return messageValue{}, errors.New("message role is missing or invalid")
	}
	content, ok := fields["content"]
	if !ok {
		return messageValue{}, errors.New("message content is missing")
	}
	// Pi's UserMessage uses a plain string for ordinary user prompts; assistant
	// and tool-result messages use content blocks. Both are valid AgentMessage
	// shapes and only assistant text blocks participate in final selection.
	if plain, decodeErr := decodeString(content); decodeErr == nil {
		message, metadataErr := decodeMessageMetadata(fields)
		if metadataErr != nil {
			return messageValue{}, metadataErr
		}
		message.role, message.text = role, []byte(plain)
		return message, nil
	}
	text, err := decodeContentBlocks(content)
	if err != nil {
		return messageValue{}, err
	}
	message, err := decodeMessageMetadata(fields)
	if err != nil {
		return messageValue{}, err
	}
	message.role, message.text = role, text
	return message, nil
}

func decodeMessageMetadata(fields map[string]json.RawMessage) (messageValue, error) {
	message := messageValue{}
	if raw, ok := fields["stopReason"]; ok {
		value, err := requiredString(map[string]json.RawMessage{"stopReason": raw}, "stopReason")
		if err != nil {
			return messageValue{}, errors.New("message stopReason is invalid")
		}
		message.stopReason = value
	}
	if raw, ok := fields["errorMessage"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		value, err := decodeString(raw)
		if err != nil || !utf8.ValidString(value) {
			return messageValue{}, errors.New("message errorMessage is invalid")
		}
		message.errorMessage = value
	}
	usage, err := parseUsage(fields)
	if err != nil {
		return messageValue{}, err
	}
	message.usage = usage
	return message, nil
}

func decodeContentBlocks(raw json.RawMessage) ([]byte, error) {
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errors.New("message content is not an array")
	}
	text := make([]byte, 0)
	for _, block := range blocks {
		if err := task.ValidateJSONStructure(block); err != nil {
			return nil, err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(block, &fields); err != nil || fields == nil {
			return nil, errors.New("message content block is not an object")
		}
		typeName, err := requiredString(fields, "type")
		if err != nil || typeName == "" {
			return nil, errors.New("message content block type is missing or invalid")
		}
		if typeName != "text" {
			continue
		}
		blockText, err := decodeString(fields["text"])
		if err != nil || !utf8.ValidString(blockText) {
			return nil, errors.New("text content block is invalid")
		}
		text = append(text, blockText...)
	}
	return text, nil
}

func decodeContentBlock(raw json.RawMessage) (string, error) {
	fields, err := decodeObject(raw)
	if err != nil {
		return "", errors.New("message content block is not an object")
	}
	typeName, err := requiredString(fields, "type")
	if err != nil {
		return "", errors.New("message content block type is missing or invalid")
	}
	if typeName != "text" {
		return "", nil
	}
	blockText, err := decodeString(fields["text"])
	if err != nil || !utf8.ValidString(blockText) {
		return "", errors.New("text content block is invalid")
	}
	return blockText, nil
}

func decodeObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New("value is not an object")
	}
	return fields, nil
}

func decodeFinalAgentMessage(raw json.RawMessage) (messageValue, error) {
	var messages []json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return messageValue{}, errors.New("messages is not an array")
	}
	var result messageValue
	seenAssistant := false
	assistantError := false
	for _, messageRaw := range messages {
		message, err := decodeMessage(messageRaw)
		if err != nil {
			return messageValue{}, err
		}
		if message.role == "assistant" {
			if message.errorMessage != "" {
				assistantError = true
			}
			if err = validateCompletedAssistantStopReason(message.stopReason); err != nil {
				return messageValue{}, err
			}
			seenAssistant = true
			result = message
		}
	}
	if !seenAssistant {
		return messageValue{}, errors.New("agent_end has no assistant message")
	}
	if assistantError && result.errorMessage == "" {
		result.errorMessage = "assistant error payload"
	}
	return result, nil
}

func validateCompletedAssistantStopReason(reason string) error {
	switch reason {
	case "stop", "toolUse", "length", "error", "aborted":
		return nil
	case "":
		return errors.New("stopReason is missing")
	default:
		return fmt.Errorf("unsupported stopReason %q", reason)
	}
}

func failedAssistantStopReason(reason string) bool {
	switch reason {
	case "length", "error", "aborted":
		return true
	default:
		return false
	}
}

func validateMessageUpdate(fields map[string]json.RawMessage) (*usageTotals, error) {
	raw, ok := fields["assistantMessageEvent"]
	if !ok {
		return nil, errors.New("message_update assistantMessageEvent is missing")
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(raw, &event); err != nil || event == nil {
		return nil, errors.New("message_update assistantMessageEvent is not an object")
	}
	typeName, err := requiredString(event, "type")
	if err != nil || typeName == "" {
		return nil, errors.New("message_update assistantMessageEvent type is invalid")
	}
	if typeName == "text_delta" {
		delta, ok := event["delta"]
		if !ok {
			return nil, errors.New("message_update text_delta is missing delta")
		}
		if _, decodeErr := decodeString(delta); decodeErr != nil {
			return nil, errors.New("message_update text_delta delta is not a string")
		}
	}
	var usage *usageTotals
	if rawUsage, ok := fields["usage"]; ok {
		usage, err = parseUsage(map[string]json.RawMessage{"usage": rawUsage})
		if err != nil {
			return nil, err
		}
	}
	return usage, nil
}

func validateToolResults(fields map[string]json.RawMessage) error {
	raw, ok := fields["toolResults"]
	if !ok {
		return errors.New("turn_end toolResults is missing")
	}
	var results []json.RawMessage
	if err := json.Unmarshal(raw, &results); err != nil {
		return errors.New("turn_end toolResults is not an array")
	}
	for _, result := range results {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(result, &object); err != nil || object == nil {
			return errors.New("turn_end toolResults contains a non-object")
		}
	}
	return nil
}

func requiredString(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok {
		return "", fmt.Errorf("%s is missing", name)
	}
	value, err := decodeString(raw)
	if err != nil || value == "" || !utf8.ValidString(value) {
		return "", fmt.Errorf("%s is not a nonempty string", name)
	}
	return value, nil
}

func decodeString(raw json.RawMessage) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}
