package pi

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/providerjson"
)

type piTextSummary struct {
	digest   [sha256.Size]byte
	length   int64
	nonblank bool
	seen     bool
}

type piSummaryWriter struct {
	hash     hash.Hash
	length   int64
	nonblank bool
}

func (w *piSummaryWriter) Write(p []byte) (int, error) {
	if strings.TrimSpace(string(p)) != "" {
		w.nonblank = true
	}
	if _, err := w.hash.Write(p); err != nil {
		return 0, err
	}
	w.length += int64(len(p))
	return len(p), nil
}

func (w *piSummaryWriter) summary() piTextSummary {
	var summary piTextSummary
	summary.length = w.length
	summary.nonblank = w.nonblank
	copy(summary.digest[:], w.hash.Sum(nil))
	summary.seen = true
	return summary
}

func summarizePiValues(values []providerjson.Value, _ bool) (piTextSummary, error) {
	writer := &piSummaryWriter{hash: sha256.New()}
	for _, value := range values {
		if err := value.CopyString(writer); err != nil {
			return piTextSummary{}, err
		}
	}
	return writer.summary(), nil
}

func equalPiText(left, right piTextSummary) bool {
	return left.seen == right.seen && left.length == right.length && left.digest == right.digest
}

func copyPiCurrentMessageText(reader io.Reader, start, end int64, out io.Writer) error {
	if start < 0 || end <= start {
		return errors.New("pi turn answer range is invalid")
	}
	var found bool
	semanticErr, readErr := providerjson.ReadRecords(reader, func(record providerjson.Value, _ uint64) error {
		if start < record.Start || end > record.End {
			return nil
		}
		found = true
		return copyPiMessageValue(providerjson.Value{Reader: record.Reader, Start: start, End: end}, out)
	})
	if err := errors.Join(semanticErr, readErr); err != nil {
		return err
	}
	if !found {
		return errors.New("selected Pi turn answer record was not found")
	}
	return nil
}

func copyPiMessageValue(message providerjson.Value, out io.Writer) error {
	fields, err := message.Object("role", "content")
	if err != nil {
		return err
	}
	content, ok := fields["content"]
	if !ok {
		return errors.New("pi turn answer content is missing")
	}
	if err := content.CopyString(out); err == nil {
		return nil
	} else {
		var fieldErr *providerjson.FieldError
		if !errors.As(err, &fieldErr) {
			return err
		}
	}
	return content.Array(func(block providerjson.Value) error {
		blockFields, err := block.Object("type")
		if err != nil {
			return err
		}
		kind, err := piCurrentRequiredString(blockFields, "type", 128)
		if err != nil {
			return err
		}
		if kind != "text" {
			return nil
		}
		textFields, err := block.Object("text")
		if err != nil {
			return err
		}
		text, ok := textFields["text"]
		if !ok {
			return errors.New("pi text content block is missing text")
		}
		return text.CopyString(out)
	})
}

func piCurrentNull(value providerjson.Value) bool {
	raw, err := value.Raw(8)
	return err == nil && bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func piCurrentRequiredString(fields map[string]providerjson.Value, name string, limit int) (string, error) {
	value, ok := fields[name]
	if !ok {
		return "", fmt.Errorf("%s is missing", name)
	}
	text, err := value.String(limit)
	if err != nil || text == "" {
		return "", fmt.Errorf("%s is not a nonempty string", name)
	}
	return text, nil
}

func piCurrentOptionalString(fields map[string]providerjson.Value, name string, limit int) (string, bool, error) {
	value, ok := fields[name]
	if !ok {
		return "", false, nil
	}
	text, err := value.String(limit)
	if err != nil {
		return "", true, err
	}
	return text, true, nil
}

func piCurrentOptionalNonnegativeInt(fields map[string]providerjson.Value, name string) (*int64, error) {
	value, ok := fields[name]
	if !ok || piCurrentNull(value) {
		return nil, nil
	}
	raw, err := value.Raw(64)
	if err != nil {
		return nil, err
	}
	number, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	if err != nil || number < 0 {
		return nil, fmt.Errorf("%s is not a nonnegative integer", name)
	}
	return &number, nil
}

type piCurrentEvent struct {
	typeName string
	fields   map[string]providerjson.Value
	value    providerjson.Value
}

func decodePiCurrentEvent(value providerjson.Value) (piCurrentEvent, error) {
	base, err := value.Object("type")
	if err != nil {
		return piCurrentEvent{}, err
	}
	typeName, err := piCurrentRequiredString(base, "type", 128)
	if err != nil {
		return piCurrentEvent{}, errors.New("event type is not a nonempty string")
	}
	names := []string{}
	switch typeName {
	case eventSession:
		names = []string{"id", "version", "cwd"}
	case eventMessageStart, eventMessageEnd:
		names = []string{"message"}
	case eventMessageUpdate:
		names = []string{"assistantMessageEvent"}
	case eventTurnEnd:
		names = []string{"message", "toolResults"}
	case eventAgentEnd:
		names = []string{"messages"}
	case eventError:
		names = []string{"error"}
	}
	fields, err := value.Object(names...)
	if err != nil {
		return piCurrentEvent{}, err
	}
	return piCurrentEvent{typeName: typeName, fields: fields, value: value}, nil
}

func parseStdout(reader io.Reader, _ ...parserOptions) (eventState, error) {
	state := eventState{current: true, currentUsageReliable: true, allowNativeRetry: true, settledTerminal: true, rejectTrailingRetryEnd: true, requireAgentEndErrorMatch: true}
	semanticErr, readErr := providerjson.ReadRecords(reader, func(value providerjson.Value, _ uint64) error {
		if state.semanticErr != nil {
			return state.semanticErr
		}
		if value.Start == value.End {
			state.markSemantic("blank JSONL event line")
			return state.semanticErr
		}
		return processPiCurrentRecord(&state, value)
	})
	if state.semanticErr == nil && semanticErr != nil {
		state.markSemantic(semanticErr.Error())
	}
	if readErr != nil {
		return state, fmt.Errorf("reading Pi stdout: %w", readErr)
	}
	return state, nil
}

func processPiCurrentRecord(state *eventState, value providerjson.Value) error {
	event, err := decodePiCurrentEvent(value)
	if err != nil {
		state.markSemantic(err.Error())
		return state.semanticErr
	}
	applyPiCurrentEvent(state, event)
	return state.semanticErr
}

func applyPiCurrentEvent(state *eventState, event piCurrentEvent) {
	if state.semanticErr != nil {
		return
	}
	if state.agentSettled && state.settledTerminal {
		state.markSemantic("event appears after agent_settled")
		return
	}
	if state.agentEnded {
		if event.typeName == eventAgentStart {
			beginPiCurrentCycle(state)
			applyPiCurrentAgentStart(state)
			return
		}
		applyPiCurrentAfterAgentEnd(state, event)
		return
	}
	applyPiCurrentBeforeAgentEnd(state, event)
}

func applyPiCurrentBeforeAgentEnd(state *eventState, event piCurrentEvent) {
	switch event.typeName {
	case eventSession:
		applyPiCurrentSession(state, event.fields)
	case eventAgentStart:
		applyPiCurrentAgentStart(state)
	case eventTurnStart:
		applyPiCurrentTurnStart(state)
	case eventMessageStart, eventMessageUpdate, eventMessageEnd:
		applyPiCurrentMessageEvent(state, event)
	case eventTurnEnd:
		applyPiCurrentTurnEnd(state, event.fields)
	case eventAgentEnd:
		applyPiCurrentAgentEnd(state, event.fields)
	case eventAutoRetryStart, eventAutoRetryEnd:
		applyPiCurrentRetryEvent(state, event.typeName)
	case eventError:
		applyPiCurrentError(state)
	default:
		applyPiCurrentUnknownEvent(state, event.typeName)
	}
}

func applyPiCurrentMessageEvent(state *eventState, event piCurrentEvent) {
	switch event.typeName {
	case eventMessageStart:
		applyPiCurrentMessageStart(state, event.fields)
	case eventMessageUpdate:
		applyPiCurrentMessageUpdate(state, event.fields, event.value)
	case eventMessageEnd:
		applyPiCurrentMessageEnd(state, event.fields)
	}
}

func applyPiCurrentRetryEvent(state *eventState, typeName string) {
	if typeName == eventAutoRetryStart {
		applyPiCurrentRetryStart(state)
		return
	}
	// Completion telemetry belongs to the active retry cycle. Its payload
	// is provider-owned and does not establish success by itself.
	if !state.agentStarted {
		state.markSemantic("auto_retry_end appears outside an active cycle")
	}
}

func applyPiCurrentError(state *eventState) {
	state.providerFail = true
	state.stickyProviderFail = true
}

func applyPiCurrentUnknownEvent(state *eventState, typeName string) {
	if strings.HasPrefix(typeName, "session_") || strings.HasPrefix(typeName, "agent_") || strings.HasPrefix(typeName, "turn_") {
		state.markSemantic("unknown lifecycle event: " + typeName)
	}
}

func applyPiCurrentAfterAgentEnd(state *eventState, event piCurrentEvent) {
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
		applyPiCurrentRetryStart(state)
	case eventAutoRetryEnd:
		if state.rejectTrailingRetryEnd && !state.providerFail && !state.retryPending {
			state.markSemantic("auto_retry_end appears after agent_end")
		}
	case eventSession, eventAgentStart, eventTurnStart, eventTurnEnd, eventMessageStart, eventMessageUpdate, eventMessageEnd:
		state.markSemantic("event appears after agent_end: " + event.typeName)
	case eventError:
		state.providerFail = true
		state.stickyProviderFail = true
	default:
		if strings.HasPrefix(event.typeName, "session_") || strings.HasPrefix(event.typeName, "agent_") || strings.HasPrefix(event.typeName, "turn_") {
			state.markSemantic("unknown lifecycle event: " + event.typeName)
		}
	}
}

func beginPiCurrentCycle(state *eventState) {
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
	state.currentLastMessage = piTextSummary{}
	state.currentTurnText = piTextSummary{}
	state.currentAgentText = piTextSummary{}
	state.currentTurnMessageStart = 0
	state.currentTurnMessageEnd = 0
	state.retryPending = false
	state.providerFail = state.stickyProviderFail
}

func applyPiCurrentSession(state *eventState, fields map[string]providerjson.Value) {
	if state.sessionSeen {
		state.markSemantic("multiple session events")
		return
	}
	if state.agentStarted || state.turnStarted {
		state.markSemantic("session event appears after agent start")
		return
	}
	id, err := piCurrentRequiredString(fields, "id", 128)
	if err != nil || !validSessionID(id) {
		state.markSemantic("session id is invalid")
		return
	}
	if _, versionErr := piCurrentOptionalNonnegativeInt(fields, "version"); versionErr != nil {
		state.markSemantic("session version is invalid")
		return
	}
	if cwd, present, cwdErr := piCurrentOptionalString(fields, "cwd", 4096); cwdErr != nil || (present && cwd == "") {
		state.markSemantic("session cwd is invalid")
		return
	}
	state.sessionID = id
	state.sessionSeen = true
}

func applyPiCurrentAgentStart(state *eventState) {
	if !state.sessionSeen || state.agentStarted || state.turnStarted {
		state.markSemantic("invalid agent_start lifecycle")
		return
	}
	state.agentStarted = true
}

func applyPiCurrentRetryStart(state *eventState) {
	if !state.agentEnded || state.retryPending {
		state.markSemantic("invalid auto_retry_start lifecycle")
		return
	}
	state.retryPending = true
}

func applyPiCurrentTurnStart(state *eventState) {
	if !state.agentStarted || (state.turnStarted && !state.turnEnded) {
		state.markSemantic("invalid turn_start lifecycle")
		return
	}
	if state.turnEnded {
		state.turnEnded = false
		state.turnStopReason = ""
		state.turnErrorMessage = ""
		state.currentLastMessage = piTextSummary{}
		state.currentTurnText = piTextSummary{}
		state.currentTurnMessageStart = 0
		state.currentTurnMessageEnd = 0
		state.lastMessageStopReason = ""
		state.lastMessageSeen = false
	}
	state.turnStarted = true
}

func applyPiCurrentMessageStart(state *eventState, fields map[string]providerjson.Value) {
	if !state.turnStarted || state.turnEnded || state.messageOpen {
		state.markSemantic("invalid message_start lifecycle")
		return
	}
	message, ok := fields["message"]
	if !ok {
		state.markSemantic("message_start message is missing")
		return
	}
	decoded, err := decodePiCurrentMessage(message, false)
	if err != nil {
		state.markSemantic("message_start message: " + err.Error())
		return
	}
	if decoded.usage != nil {
		state.currentUsagePresent = true
		state.currentUsageReliable = state.currentUsageReliable && decoded.usageReliable
	}
	state.messageUsage = nil
	state.messageUsageReliable = true
	state.messageOpen = true
}

func applyPiCurrentMessageUpdate(state *eventState, fields map[string]providerjson.Value, value providerjson.Value) {
	if !state.turnStarted || state.turnEnded {
		state.markSemantic("invalid message_update lifecycle")
		return
	}
	event, ok := fields["assistantMessageEvent"]
	if !ok {
		state.markSemantic("message_update assistantMessageEvent is missing")
		return
	}
	eventFields, err := event.Object("type", "delta")
	if err != nil {
		state.markSemantic("message_update assistantMessageEvent is not an object")
		return
	}
	typeName, err := piCurrentRequiredString(eventFields, "type", 128)
	if err != nil {
		state.markSemantic("message_update assistantMessageEvent type is invalid")
		return
	}
	if typeName == "text_delta" {
		delta, ok := eventFields["delta"]
		if !ok {
			state.markSemantic("message_update text_delta is missing delta")
			return
		}
		if _, err := summarizePiValues([]providerjson.Value{delta}, false); err != nil {
			state.markSemantic("message_update text_delta delta is not a string")
			return
		}
	}
	usageFields, usageErr := value.Object("usage")
	if usageErr != nil {
		state.messageUsage = &usageTotals{present: true}
		state.messageUsageReliable = false
		state.currentUsagePresent = true
		state.currentUsageReliable = false
	} else if usage, reliable := parsePiCurrentUsage(usageFields["usage"]); usage != nil {
		state.messageUsage = usage
		state.messageUsageReliable = reliable
		state.currentUsagePresent = true
		state.currentUsageReliable = state.currentUsageReliable && reliable
	}
}

func applyPiCurrentMessageEnd(state *eventState, fields map[string]providerjson.Value) {
	if !state.turnStarted || state.turnEnded || !state.messageOpen {
		state.markSemantic("invalid message_end lifecycle")
		return
	}
	value, ok := fields["message"]
	if !ok {
		state.markSemantic("message_end message is missing")
		return
	}
	message, err := decodePiCurrentMessage(value, true)
	if err != nil {
		state.markSemantic("message_end message: " + err.Error())
		return
	}
	state.messageOpen = false
	if message.role != "assistant" {
		return
	}
	if message.errorMessage != "" {
		state.providerFail = true
	}
	if err := validateCompletedAssistantStopReason(message.stopReason); err != nil {
		state.markSemantic("message_end assistant stop reason: " + err.Error())
		return
	}
	if failedAssistantStopReason(message.stopReason) {
		state.providerFail = true
	}
	if message.usage != nil {
		appendPiCurrentUsage(state, message.usage, message.usageReliable)
	} else if state.messageUsage != nil {
		appendPiCurrentUsage(state, state.messageUsage, state.messageUsageReliable)
	}
	state.messageUsage = nil
	state.messageUsageReliable = true
	state.currentLastMessage = message.text
	state.lastMessageSeen = true
	state.lastMessageStopReason = message.stopReason
}

// Current usage is optional provider telemetry. Keep a bounded receipt for
// usable rows, but never turn an unreliable usage overflow into a malformed
// answer.
func appendPiCurrentUsage(state *eventState, usage *usageTotals, reliable bool) {
	if usage == nil {
		return
	}
	state.currentUsagePresent = true
	state.currentUsageReliable = state.currentUsageReliable && reliable
	if len(state.usage) >= maxUsageRows {
		state.currentUsageReliable = false
		return
	}
	state.usage = append(state.usage, usage)
}

func applyPiCurrentTurnEnd(state *eventState, fields map[string]providerjson.Value) {
	if !state.turnStarted || state.turnEnded || state.messageOpen {
		state.markSemantic("invalid turn_end lifecycle")
		return
	}
	value, ok := fields["message"]
	if !ok {
		state.markSemantic("turn_end message is missing")
		return
	}
	message, err := decodePiCurrentMessage(value, true)
	if err != nil {
		state.markSemantic("turn_end message: " + err.Error())
		return
	}
	if err := validatePiCurrentToolResults(fields); err != nil {
		state.markSemantic(err.Error())
		return
	}
	if message.usage != nil {
		state.currentUsagePresent = true
		state.currentUsageReliable = state.currentUsageReliable && message.usageReliable
	}
	if err := state.validatePiCurrentTurnEndMessage(message); err != nil {
		state.markSemantic(err.Error())
		return
	}
	state.currentTurnText = message.text
	state.currentTurnMessageStart = value.Start
	state.currentTurnMessageEnd = value.End
	state.turnStopReason = message.stopReason
	state.turnErrorMessage = message.errorMessage
	state.turnEnded = true
}

func (state *eventState) validatePiCurrentTurnEndMessage(message piCurrentMessage) error {
	if message.role != "assistant" {
		return errors.New("turn_end message is not an assistant message")
	}
	if message.errorMessage != "" {
		state.providerFail = true
	}
	if err := validateCompletedAssistantStopReason(message.stopReason); err != nil {
		return errors.New("turn_end assistant stop reason: " + err.Error())
	}
	if failedAssistantStopReason(message.stopReason) {
		state.providerFail = true
	}
	if state.currentLastMessage.seen && !equalPiText(state.currentLastMessage, message.text) {
		return errors.New("turn_end assistant text conflicts with message_end")
	}
	if state.lastMessageStopReason != "" && state.lastMessageStopReason != message.stopReason {
		return errors.New("turn_end assistant stop reason conflicts with message_end")
	}
	return nil
}

func applyPiCurrentAgentEnd(state *eventState, fields map[string]providerjson.Value) {
	if !state.turnEnded || state.agentEnded {
		state.markSemantic("invalid agent_end lifecycle")
		return
	}
	value, ok := fields["messages"]
	if !ok {
		state.markSemantic("agent_end messages are missing")
		return
	}
	message, assistantError, err := decodePiCurrentFinalMessage(value)
	if err != nil {
		state.markSemantic("agent_end messages: " + err.Error())
		return
	}
	if message.usage != nil {
		state.currentUsagePresent = true
		state.currentUsageReliable = state.currentUsageReliable && message.usageReliable
	}
	if message.errorMessage != "" || assistantError {
		state.providerFail = true
	}
	if state.turnStopReason != message.stopReason {
		state.markSemantic("agent_end assistant stop reason conflicts with turn_end")
		return
	}
	if !equalPiText(state.currentTurnText, message.text) {
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
	state.currentAgentText = message.text
	state.agentEnded = true
}

type piCurrentMessage struct {
	role          string
	text          piTextSummary
	stopReason    string
	errorMessage  string
	usage         *usageTotals
	usageReliable bool
}

func decodePiCurrentMessage(value providerjson.Value, keepText bool) (piCurrentMessage, error) {
	fields, err := value.Object("role", "content", "stopReason", "errorMessage")
	if err != nil {
		return piCurrentMessage{}, errors.New("message is not an object")
	}
	role, err := piCurrentRequiredString(fields, "role", 128)
	if err != nil {
		return piCurrentMessage{}, errors.New("message role is missing or invalid")
	}
	content, ok := fields["content"]
	if !ok {
		return piCurrentMessage{}, errors.New("message content is missing")
	}
	var text piTextSummary
	if plain, plainErr := summarizePiValues([]providerjson.Value{content}, keepText && role == "assistant"); plainErr == nil {
		text = plain
	} else {
		blocks, arrayErr := decodePiCurrentContentBlocks(content, keepText && role == "assistant")
		if arrayErr != nil {
			return piCurrentMessage{}, arrayErr
		}
		text = blocks
	}
	stopReason := ""
	if raw, present := fields["stopReason"]; present {
		value, stringErr := raw.String(128)
		if stringErr != nil || value == "" {
			return piCurrentMessage{}, errors.New("message stopReason is invalid")
		}
		stopReason = value
	}
	errorMessage := ""
	if raw, present := fields["errorMessage"]; present && !piCurrentNull(raw) {
		value, stringErr := raw.String(maxEventLineBytes)
		if stringErr != nil {
			return piCurrentMessage{}, errors.New("message errorMessage is invalid")
		}
		errorMessage = value
	}
	usageFields, usageErr := value.Object("usage")
	usage := (*usageTotals)(nil)
	var usageReliable bool
	if usageErr != nil {
		usage = &usageTotals{present: true}
		usageReliable = false
	} else {
		usage, usageReliable = parsePiCurrentUsage(usageFields["usage"])
	}
	return piCurrentMessage{role: role, text: text, stopReason: stopReason, errorMessage: errorMessage, usage: usage, usageReliable: usageReliable}, nil
}

func decodePiCurrentContentBlocks(value providerjson.Value, keepText bool) (piTextSummary, error) {
	if piCurrentNull(value) {
		return piTextSummary{}, errors.New("message content is not an array")
	}
	var writer *piSummaryWriter
	if keepText {
		writer = &piSummaryWriter{hash: sha256.New()}
	}
	if err := value.Array(func(block providerjson.Value) error {
		fields, err := block.Object("type")
		if err != nil {
			return errors.New("message content block is not an object")
		}
		kind, err := piCurrentRequiredString(fields, "type", 128)
		if err != nil {
			return errors.New("message content block type is missing or invalid")
		}
		if kind != "text" {
			return nil
		}
		fields, err = block.Object("text")
		if err != nil {
			return fmt.Errorf("text content block: %w", err)
		}
		text, ok := fields["text"]
		if !ok {
			return errors.New("text content block is invalid")
		}
		if keepText {
			return text.CopyString(writer)
		}
		if _, err := summarizePiValues([]providerjson.Value{text}, false); err != nil {
			return errors.New("text content block is invalid")
		}
		return nil
	}); err != nil {
		return piTextSummary{}, err
	}
	if !keepText {
		return piTextSummary{seen: true, digest: sha256.Sum256(nil)}, nil
	}
	return writer.summary(), nil
}

func decodePiCurrentFinalMessage(value providerjson.Value) (piCurrentMessage, bool, error) {
	if piCurrentNull(value) {
		return piCurrentMessage{}, false, errors.New("messages is not an array")
	}
	var result piCurrentMessage
	seenAssistant := false
	assistantError := false
	if err := value.Array(func(element providerjson.Value) error {
		message, err := decodePiCurrentMessage(element, true)
		if err != nil {
			return err
		}
		if message.role != "assistant" {
			return nil
		}
		if message.errorMessage != "" {
			assistantError = true
		}
		if err := validateCompletedAssistantStopReason(message.stopReason); err != nil {
			return err
		}
		seenAssistant = true
		result = message
		return nil
	}); err != nil {
		return piCurrentMessage{}, false, err
	}
	if !seenAssistant {
		return piCurrentMessage{}, false, errors.New("agent_end has no assistant message")
	}
	if assistantError && result.errorMessage == "" {
		result.errorMessage = "assistant error payload"
	}
	return result, assistantError, nil
}

func validatePiCurrentToolResults(fields map[string]providerjson.Value) error {
	value, ok := fields["toolResults"]
	if !ok || piCurrentNull(value) {
		return errors.New("turn_end toolResults is missing")
	}
	if err := value.Array(func(element providerjson.Value) error {
		if piCurrentNull(element) {
			return errors.New("turn_end toolResults contains a non-object")
		}
		_, err := element.Object()
		if err != nil {
			return errors.New("turn_end toolResults contains a non-object")
		}
		return nil
	}); err != nil {
		return errors.New("turn_end toolResults is not an array")
	}
	return nil
}

func parsePiCurrentUsage(value providerjson.Value) (*usageTotals, bool) {
	if value.Reader == nil {
		return nil, true
	}
	usage := &usageTotals{present: true}
	if piCurrentNull(value) {
		return usage, false
	}
	fields, err := value.Object("input_tokens", "inputTokens", "input", "output_tokens", "outputTokens", "output", "thinking_tokens", "thinkingTokens", "thinking", "total_tokens", "totalTokens", "total", "cache_read_input_tokens", "cacheReadInputTokens", "cacheRead", "cache_creation_input_tokens", "cacheCreationInputTokens", "cacheWrite")
	if err != nil {
		return usage, false
	}
	reliable := true
	known := 0
	usage.input, reliable = piCurrentUsageCounter(fields, reliable, &known, "input_tokens", "inputTokens", "input")
	usage.output, reliable = piCurrentUsageCounter(fields, reliable, &known, "output_tokens", "outputTokens", "output")
	usage.thinking, reliable = piCurrentUsageCounter(fields, reliable, &known, "thinking_tokens", "thinkingTokens", "thinking")
	usage.total, reliable = piCurrentUsageCounter(fields, reliable, &known, "total_tokens", "totalTokens", "total")
	usage.cacheRead, reliable = piCurrentUsageCounter(fields, reliable, &known, "cache_read_input_tokens", "cacheReadInputTokens", "cacheRead")
	usage.cacheCreation, reliable = piCurrentUsageCounter(fields, reliable, &known, "cache_creation_input_tokens", "cacheCreationInputTokens", "cacheWrite")
	if known == 0 {
		reliable = false
	}
	return usage, reliable
}

func piCurrentUsageCounter(fields map[string]providerjson.Value, reliable bool, known *int, names ...string) (*int64, bool) {
	for _, name := range names {
		if value, ok := fields[name]; ok {
			(*known)++
			if piCurrentNull(value) {
				return nil, false
			}
			raw, err := value.Raw(64)
			if err != nil {
				return nil, false
			}
			number, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
			if err != nil || number < 0 {
				return nil, false
			}
			return &number, reliable
		}
	}
	return nil, reliable
}
