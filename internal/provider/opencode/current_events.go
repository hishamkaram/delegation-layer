package opencode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/providerjson"
)

type opencodeTextSummary struct {
	length   int64
	nonblank bool
	seen     bool
}

func summarizeOpenCodeValue(value providerjson.Value) (opencodeTextSummary, error) {
	var summary opencodeTextSummary
	writer := opencodeSummaryWriter{summary: &summary}
	if err := value.CopyString(&writer); err != nil {
		return opencodeTextSummary{}, err
	}
	summary.seen = true
	return summary, nil
}

type opencodeSummaryWriter struct{ summary *opencodeTextSummary }

func (w *opencodeSummaryWriter) Write(p []byte) (int, error) {
	w.summary.length += int64(len(p))
	if strings.TrimSpace(string(p)) != "" {
		w.summary.nonblank = true
	}
	return len(p), nil
}

func appendOpenCodeText(dst *opencodeTextSummary, src opencodeTextSummary) {
	if !src.seen {
		return
	}
	dst.seen = true
	dst.length += src.length
	dst.nonblank = dst.nonblank || src.nonblank
}

func copyOpenCodeCurrentAnswer(reader io.Reader, start, end int64, out io.Writer) error {
	if start < 0 || end <= start {
		return errors.New("OpenCode terminal answer range is invalid")
	}
	semanticErr, readErr := providerjson.ReadRecords(reader, func(value providerjson.Value, _ uint64) error {
		if value.Start < start || value.End > end {
			return nil
		}
		event, err := decodeOpenCodeCurrentEvent(value)
		if err != nil {
			return err
		}
		if event.typeName != eventText {
			return nil
		}
		part, err := openCodeCurrentPart(event.fields, "text", "text")
		if err != nil {
			return err
		}
		text, ok := part["text"]
		if !ok {
			return errors.New("text event part.text is missing")
		}
		return text.CopyString(out)
	})
	return errors.Join(semanticErr, readErr)
}

func openCodeCurrentNull(value providerjson.Value) bool {
	raw, err := value.Raw(8)
	return err == nil && bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func openCodeCurrentRequiredString(fields map[string]providerjson.Value, name string, limit int) (string, error) {
	value, ok := fields[name]
	if !ok {
		return "", fmt.Errorf("missing %s", name)
	}
	text, err := value.String(limit)
	if err != nil {
		return "", fmt.Errorf("%s is not a string", name)
	}
	return text, nil
}

func openCodeCurrentOptionalString(fields map[string]providerjson.Value, name string, limit int) (string, bool, error) {
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

type openCodeCurrentEvent struct {
	typeName string
	fields   map[string]providerjson.Value
}

func decodeOpenCodeCurrentEvent(value providerjson.Value) (openCodeCurrentEvent, error) {
	base, err := value.Object("type", "sessionID")
	if err != nil {
		return openCodeCurrentEvent{}, err
	}
	typeName, err := openCodeCurrentRequiredString(base, "type", 128)
	if err != nil || typeName == "" {
		return openCodeCurrentEvent{}, errors.New("event type is not a nonempty string")
	}
	names := []string{"sessionID"}
	switch typeName {
	case eventStepStart, eventText, eventReasoning, eventToolUse, eventStepFinish:
		names = append(names, "part")
	case eventError:
		names = append(names, "error")
	}
	fields, err := value.Object(names...)
	if err != nil {
		return openCodeCurrentEvent{}, err
	}
	return openCodeCurrentEvent{typeName: typeName, fields: fields}, nil
}

func parseStdout(reader io.Reader) (eventState, error) {
	return parseStdoutWithPostTerminal(reader, true, true)
}

func parseStrictCurrentStdout(reader io.Reader) (eventState, error) {
	return parseStdoutWithPostTerminal(reader, false, false)
}

func parseHistoricalCurrentV4Stdout(reader io.Reader) (eventState, error) {
	return parseStdoutWithPostTerminal(reader, true, false)
}

func parseStdoutWithPostTerminal(reader io.Reader, allowPostTerminal, allowEmptyStepStart bool) (eventState, error) {
	state := eventState{current: true, currentUsageReliable: true, allowPostTerminal: allowPostTerminal, allowEmptyStepStart: allowEmptyStepStart}
	semanticErr, readErr := providerjson.ReadRecords(reader, func(value providerjson.Value, _ uint64) error {
		if state.semanticErr != nil {
			return state.semanticErr
		}
		if value.Start == value.End {
			state.markSemantic("blank JSONL event line")
			return state.semanticErr
		}
		return processOpenCodeCurrentRecord(&state, value)
	})
	if state.semanticErr == nil && semanticErr != nil {
		state.markSemantic(semanticErr.Error())
	}
	if readErr != nil {
		return state, fmt.Errorf("reading OpenCode stdout: %w", readErr)
	}
	return state, nil
}

func processOpenCodeCurrentRecord(state *eventState, value providerjson.Value) error {
	event, err := decodeOpenCodeCurrentEvent(value)
	if err != nil {
		state.markSemantic(err.Error())
		return state.semanticErr
	}
	if event.typeName == eventStepStart {
		state.currentStepStart = value.Start
	}
	applyOpenCodeCurrentEvent(state, event)
	if event.typeName == eventStepFinish && state.currentStepTerminal {
		state.currentTerminalStart = state.currentStepStart
		state.currentTerminalEnd = value.End
	}
	return state.semanticErr
}

func applyOpenCodeCurrentEvent(state *eventState, event openCodeCurrentEvent) {
	if state.semanticErr != nil {
		return
	}
	if state.terminal && !state.allowPostTerminal {
		state.markSemantic("event follows terminal step_finish")
		return
	}
	sessionID, err := openCodeCurrentRequiredString(event.fields, "sessionID", 128)
	if err != nil || !validSessionID(sessionID) {
		state.markSemantic("event sessionID is invalid")
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
		applyOpenCodeCurrentStepStart(state, event.fields)
	case eventText:
		applyOpenCodeCurrentText(state, event.fields)
	case eventReasoning:
		applyOpenCodeCurrentReasoning(state, event.fields)
	case eventToolUse:
		applyOpenCodeCurrentToolUse(state, event.fields)
	case eventStepFinish:
		applyOpenCodeCurrentStepFinish(state, event.fields)
	case eventError:
		applyOpenCodeCurrentError(state, event.fields)
	default:
		state.markSemantic("unknown event type: " + event.typeName)
	}
}

func openCodeCurrentPart(fields map[string]providerjson.Value, expected string, names ...string) (map[string]providerjson.Value, error) {
	part, ok := fields["part"]
	if !ok || openCodeCurrentNull(part) {
		return nil, fmt.Errorf("missing part object")
	}
	selected := append([]string{"type"}, names...)
	partFields, err := part.Object(selected...)
	if err != nil {
		return nil, errors.New("part is not an object")
	}
	if partType, present, typeErr := openCodeCurrentOptionalString(partFields, "type", 128); typeErr != nil || (present && partType != expected) {
		return nil, fmt.Errorf("part type is not %s", expected)
	}
	return partFields, nil
}

func applyOpenCodeCurrentStepStart(state *eventState, fields map[string]providerjson.Value) {
	if state.stepActive {
		if state.currentStepHadEvent || !state.allowEmptyStepStart {
			state.markSemantic("duplicate step_start after step content")
			return
		}
		// OpenCode can emit an empty step_start while replacing a
		// provider-internal step. Treat the repeated empty marker as the
		// start of the same logical step; any meaningful event keeps the
		// stricter duplicate-start rejection above.
	}
	part, err := openCodeCurrentPart(fields, "step-start")
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	state.stepStarted = true
	state.stepActive = true
	state.terminal = false
	state.currentStepTerminal = false
	state.currentStepHadEvent = false
	state.currentStepText = opencodeTextSummary{}
	_ = part
}

func applyOpenCodeCurrentText(state *eventState, fields map[string]providerjson.Value) {
	if !state.stepActive {
		state.markSemantic("text event is outside an active step")
		return
	}
	state.currentStepHadEvent = true
	part, err := openCodeCurrentPart(fields, "text", "text")
	if err != nil {
		state.markSemantic("text event: " + err.Error())
		return
	}
	text, ok := part["text"]
	if !ok {
		state.markSemantic("text event part.text is not a string")
		return
	}
	summary, err := summarizeOpenCodeValue(text)
	if err != nil {
		state.markSemantic("text event part.text is not a string")
		return
	}
	appendOpenCodeText(&state.currentStepText, summary)
}

func applyOpenCodeCurrentReasoning(state *eventState, fields map[string]providerjson.Value) {
	if !state.stepActive {
		state.markSemantic("reasoning event is outside an active step")
		return
	}
	state.currentStepHadEvent = true
	part, err := openCodeCurrentPart(fields, "reasoning", "text")
	if err != nil {
		state.markSemantic("reasoning event: " + err.Error())
		return
	}
	text, ok := part["text"]
	if !ok {
		state.markSemantic("reasoning event part.text is not a string")
		return
	}
	if _, err := summarizeOpenCodeValue(text); err != nil {
		state.markSemantic("reasoning event part.text is not a string")
	}
}

func applyOpenCodeCurrentToolUse(state *eventState, fields map[string]providerjson.Value) {
	if !state.stepActive {
		state.markSemantic("tool_use event is outside an active step")
		return
	}
	state.currentStepHadEvent = true
	part, err := openCodeCurrentPart(fields, "tool", "tool", "state")
	if err != nil {
		state.markSemantic("tool_use event: " + err.Error())
		return
	}
	tool, err := openCodeCurrentRequiredString(part, "tool", 256)
	if err != nil || strings.TrimSpace(tool) == "" {
		state.markSemantic("tool_use event tool is not a nonempty string")
		return
	}
	toolState, ok := part["state"]
	if !ok || openCodeCurrentNull(toolState) {
		state.markSemantic("tool_use event state is not an object")
		return
	}
	stateFields, err := toolState.Object("status", "error")
	if err != nil {
		state.markSemantic("tool_use event state is not an object")
		return
	}
	status, err := openCodeCurrentRequiredString(stateFields, "status", 64)
	if err != nil {
		state.markSemantic("tool_use event state status is missing or invalid")
		return
	}
	switch status {
	case "completed":
		return
	case "error":
		if diagnostic, present := stateFields["error"]; present {
			if _, err := summarizeOpenCodeValue(diagnostic); err != nil {
				state.markSemantic("tool_use error diagnostic is invalid")
			}
		}
	default:
		state.markSemantic("tool_use event state has unknown status")
	}
}

func applyOpenCodeCurrentStepFinish(state *eventState, fields map[string]providerjson.Value) {
	if !state.stepActive {
		state.markSemantic("step_finish is outside an active step")
		return
	}
	part, err := openCodeCurrentPart(fields, "step-finish", "reason")
	if err != nil {
		state.markSemantic("step_finish: " + err.Error())
		return
	}
	reason, err := openCodeCurrentRequiredString(part, "reason", 64)
	if err != nil || (reason != "stop" && reason != "tool-calls") {
		state.markSemantic("step_finish has unknown reason")
		return
	}
	partValue := fields["part"]
	usageFields, usageErr := partValue.Object("tokens", "cost")
	if usageErr != nil {
		// Optional accounting must not turn a valid stopped step into a
		// malformed answer. A duplicate selected usage member is unreliable
		// metadata, not lifecycle authority.
		appendOpenCodeCurrentUsage(state, &usageTotals{present: true}, false)
	} else if usage, reliable := parseOpenCodeCurrentUsage(usageFields); usage != nil {
		appendOpenCodeCurrentUsage(state, usage, reliable)
	}
	state.stepActive = false
	state.currentStepTerminal = reason == "stop"
	if reason == "stop" {
		state.terminal = true
		state.currentTerminalText = state.currentStepText
		state.terminalAnswerSeen = state.currentStepText.seen
	}
}

// Usage is optional provider telemetry. Retain only the bounded prefix of
// usable rows; malformed or excessive telemetry must not reject an otherwise
// valid final answer.
func appendOpenCodeCurrentUsage(state *eventState, usage *usageTotals, reliable bool) {
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

func applyOpenCodeCurrentError(state *eventState, fields map[string]providerjson.Value) {
	errorValue, ok := fields["error"]
	if !ok || openCodeCurrentNull(errorValue) {
		state.providerFail = true
		return
	}
	errorFields, err := errorValue.Object("name")
	if err != nil {
		state.markSemantic("error event error is not an object")
		return
	}
	if name, err := openCodeCurrentRequiredString(errorFields, "name", 256); err != nil || strings.TrimSpace(name) == "" {
		state.markSemantic("error event name is not a nonempty string")
		return
	}
	state.providerFail = true
}

func parseOpenCodeCurrentUsage(part map[string]providerjson.Value) (*usageTotals, bool) {
	usage := &usageTotals{}
	reliable := true
	known := 0
	if tokens, present := part["tokens"]; present {
		usage.present = true
		reliable = parseOpenCodeCurrentTokenUsage(usage, tokens, reliable, &known)
	}
	if cost, present := part["cost"]; present {
		usage.present = true
		reliable = parseOpenCodeCurrentCost(usage, cost, reliable, &known)
	}
	if !usage.present {
		return nil, true
	}
	if known == 0 {
		reliable = false
	}
	return usage, reliable
}

func parseOpenCodeCurrentTokenUsage(usage *usageTotals, tokens providerjson.Value, reliable bool, known *int) bool {
	if openCodeCurrentNull(tokens) {
		return false
	}
	fields, err := tokens.Object("input", "output", "reasoning", "total", "cache")
	if err != nil {
		return false
	}
	usage.input, reliable = openCodeCurrentCounter(fields, reliable, known, "input")
	usage.output, reliable = openCodeCurrentCounter(fields, reliable, known, "output")
	usage.reasoning, reliable = openCodeCurrentCounter(fields, reliable, known, "reasoning")
	usage.total, reliable = openCodeCurrentCounter(fields, reliable, known, "total")
	if cache, present := fields["cache"]; present {
		reliable = parseOpenCodeCurrentCacheUsage(usage, cache, reliable, known)
	}
	return reliable
}

func parseOpenCodeCurrentCacheUsage(usage *usageTotals, cache providerjson.Value, reliable bool, known *int) bool {
	if openCodeCurrentNull(cache) {
		(*known)++
		return false
	}
	fields, err := cache.Object("read", "write")
	if err != nil {
		return false
	}
	usage.cacheRead, reliable = openCodeCurrentCounter(fields, reliable, known, "read")
	usage.cacheWrite, reliable = openCodeCurrentCounter(fields, reliable, known, "write")
	return reliable
}

func parseOpenCodeCurrentCost(usage *usageTotals, cost providerjson.Value, reliable bool, known *int) bool {
	if openCodeCurrentNull(cost) {
		return false
	}
	raw, err := cost.Raw(128)
	if err != nil {
		return false
	}
	text := string(bytes.TrimSpace(raw))
	if _, err := decimalNumber(json.RawMessage([]byte(text))); err != nil {
		return false
	}
	usage.cost = &text
	(*known)++
	return reliable
}

func openCodeCurrentCounter(fields map[string]providerjson.Value, reliable bool, known *int, name string) (*int64, bool) {
	value, ok := fields[name]
	if !ok {
		return nil, reliable
	}
	(*known)++
	if openCodeCurrentNull(value) {
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
