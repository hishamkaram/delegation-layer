package claude

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/providerjson"
)

func claudeCurrentRequiredString(fields map[string]providerjson.Value, name string, limit int) (string, error) {
	value, ok := fields[name]
	if !ok {
		return "", fmt.Errorf("missing %s", name)
	}
	text, err := value.String(limit)
	if err != nil {
		return "", fmt.Errorf("%s is not a string: %w", name, err)
	}
	return text, nil
}

func claudeCurrentOptionalString(fields map[string]providerjson.Value, name string, limit int) (string, bool, error) {
	value, ok := fields[name]
	if !ok {
		return "", false, nil
	}
	text, err := value.String(limit)
	if err != nil {
		return "", true, fmt.Errorf("%s is not a string: %w", name, err)
	}
	return text, true, nil
}

func claudeCurrentRequiredText(fields map[string]providerjson.Value, name string, limit int) (string, error) {
	text, err := claudeCurrentRequiredString(fields, name, limit)
	if err != nil || !validText(text) {
		return "", fmt.Errorf("%s is invalid", name)
	}
	return text, nil
}

func claudeCurrentValueIsNull(value providerjson.Value) bool {
	raw, err := value.Raw(8)
	return err == nil && bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func claudeCurrentRequiredBool(fields map[string]providerjson.Value, name string) (bool, error) {
	value, ok := fields[name]
	if !ok {
		return false, fmt.Errorf("missing %s", name)
	}
	raw, err := value.Raw(8)
	if err != nil {
		return false, err
	}
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, errors.New("value is not a JSON boolean")
	}
}

func claudeCurrentOptionalNonnegativeInt(fields map[string]providerjson.Value, name string) (*int64, error) {
	value, ok := fields[name]
	if !ok || claudeCurrentValueIsNull(value) {
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

func claudeCurrentOptionalDecimal(fields map[string]providerjson.Value, name string) (*string, error) {
	value, ok := fields[name]
	if !ok || claudeCurrentValueIsNull(value) {
		return nil, nil
	}
	raw, err := value.Raw(128)
	if err != nil {
		return nil, err
	}
	text := string(bytes.TrimSpace(raw))
	if err := validateDecimal(text); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &text, nil
}

func claudeCurrentEventType(value providerjson.Value) (string, error) {
	fields, err := value.Object("type")
	if err != nil {
		return "", err
	}
	typeName, err := claudeCurrentRequiredText(fields, "type", 256)
	if err != nil {
		return "", errors.New("event type is not a nonempty string")
	}
	return typeName, nil
}

type claudeCurrentEvent struct {
	typeName string
	fields   map[string]providerjson.Value
	value    providerjson.Value
}

func claudeCurrentEventFields(value providerjson.Value, typeName string) (map[string]providerjson.Value, error) {
	names := []string{"session_id"}
	switch typeName {
	case "system":
		names = append(names, "subtype", "claude_code_version", "permissionMode", "cwd", "apiKeySource", "tools", "mcp_servers", "model", "state", "task_type", "task_id", "patch")
	case "result":
		names = append(names, "subtype", "is_error", "result", "stop_reason", "errors", "permission_denials")
	case "assistant", "user":
		names = append(names, "message")
	case "control_response":
		names = append(names, "response")
	case "control_request":
		names = append(names, "request_id", "request")
	case "control_cancel_request":
		names = append(names, "request_id")
	}
	return value.Object(names...)
}

func decodeClaudeCurrentEvent(value providerjson.Value) (claudeCurrentEvent, error) {
	if err := value.ValidateUniqueKeys(); err != nil {
		return claudeCurrentEvent{}, err
	}
	typeName, err := claudeCurrentEventType(value)
	if err != nil {
		return claudeCurrentEvent{}, err
	}
	fields, err := claudeCurrentEventFields(value, typeName)
	if err != nil {
		return claudeCurrentEvent{}, err
	}
	return claudeCurrentEvent{typeName: typeName, fields: fields, value: value}, nil
}

func parseStdout(reader io.Reader) (eventState, error) {
	return parseCurrentClaudeStdoutForMode(reader, Mode)
}

func parseStdoutForMode(reader io.Reader, mode string) (eventState, error) {
	return parseCurrentClaudeStdoutForMode(reader, mode)
}

func parseCurrentClaudeStdoutForMode(reader io.Reader, mode string) (eventState, error) {
	state := eventState{mode: mode, current: true}
	if mode != Mode && mode != WorkspaceWriteMode {
		state.semanticErr = fmt.Errorf("unsupported Claude permission mode %q", mode)
		return state, nil
	}
	semanticErr, readErr := providerjson.ReadRecords(reader, func(value providerjson.Value, _ uint64) error {
		if state.semanticErr != nil {
			return state.semanticErr
		}
		if value.Start == value.End {
			state.markSemantic("blank JSONL event line")
			return state.semanticErr
		}
		return processClaudeCurrentRecord(&state, value)
	})
	if state.semanticErr == nil && semanticErr != nil {
		state.markSemantic(semanticErr.Error())
	}
	if readErr != nil {
		return state, fmt.Errorf("reading Claude stdout: %w", readErr)
	}
	return state, nil
}

func processClaudeCurrentRecord(state *eventState, value providerjson.Value) error {
	event, err := decodeClaudeCurrentEvent(value)
	if err != nil {
		state.markSemantic(err.Error())
		return state.semanticErr
	}
	applyClaudeCurrentEvent(state, event)
	return state.semanticErr
}

func bindClaudeCurrentSession(state *eventState, event claudeCurrentEvent) bool {
	value, ok := event.fields["session_id"]
	if !ok {
		return true
	}
	sessionID, err := value.String(128)
	if err != nil || !validSessionID(sessionID) {
		state.markSemantic(event.typeName + " session identity is invalid")
		return false
	}
	if state.observedSessionID != "" && state.observedSessionID != sessionID {
		state.markSemantic(event.typeName + " session identity changed")
		return false
	}
	if state.initSeen && state.init.sessionID != sessionID {
		state.markSemantic(event.typeName + " session identity conflicts with system/init")
		return false
	}
	state.observedSessionID = sessionID
	return true
}

func applyClaudeCurrentEvent(state *eventState, event claudeCurrentEvent) {
	if state.semanticErr != nil || !bindClaudeCurrentSession(state, event) {
		return
	}
	switch event.typeName {
	case "system":
		applyClaudeCurrentSystem(state, event.fields)
	case "result":
		applyClaudeCurrentResult(state, event.fields, event.value)
	case "assistant":
		applyClaudeCurrentMessageEvent(state, event.fields, "assistant")
	case "user":
		applyClaudeCurrentMessageEvent(state, event.fields, "user")
	case "error":
		state.markSemantic("top-level error event is terminally ambiguous")
	default:
		if strings.HasPrefix(event.typeName, "thread.") || strings.HasPrefix(event.typeName, "turn.") || strings.HasPrefix(event.typeName, "session.") {
			state.markSemantic("unknown thread/turn/session lifecycle event: " + event.typeName)
		}
	}
}

func applyClaudeCurrentSystem(state *eventState, fields map[string]providerjson.Value) {
	subtype, present, err := claudeCurrentOptionalString(fields, "subtype", 256)
	if err != nil {
		state.markSemantic("system subtype is not a string")
		return
	}
	if !present || subtype != "init" {
		if ambiguousSystemSubtype(subtype) {
			state.markSemantic("unknown terminal system lifecycle event: " + subtype)
		}
		return
	}
	if state.resultSeen {
		state.markSemantic("system/init follows result")
		return
	}
	if state.initSeen {
		state.markSemantic("multiple system/init events")
		return
	}
	init, err := decodeClaudeCurrentInit(fields, state.mode)
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	state.initSeen = true
	state.init = init
	state.observedSessionID = init.sessionID
}

func decodeClaudeCurrentInit(fields map[string]providerjson.Value, mode string) (initState, error) {
	sessionID, err := claudeCurrentRequiredString(fields, "session_id", 128)
	if err != nil || !validSessionID(sessionID) {
		return initState{}, errors.New("system/init session_id is not a UUID")
	}
	version, err := claudeCurrentRequiredText(fields, "claude_code_version", 256)
	if err != nil {
		return initState{}, errors.New("system/init claude_code_version is invalid")
	}
	permission, err := claudeCurrentRequiredText(fields, "permissionMode", 128)
	if err != nil {
		return initState{}, err
	}
	wantPermission := "plan"
	if mode == WorkspaceWriteMode {
		wantPermission = "bypassPermissions"
	} else if mode != Mode {
		return initState{}, fmt.Errorf("unsupported Claude permission mode %q", mode)
	}
	if permission != wantPermission {
		return initState{}, fmt.Errorf("system/init permissionMode is not %s", wantPermission)
	}
	apiKeySource, err := claudeCurrentRequiredText(fields, "apiKeySource", 256)
	if err != nil {
		return initState{}, errors.New("system/init apiKeySource is invalid")
	}
	cwd, err := claudeCurrentRequiredText(fields, "cwd", 4096)
	if err != nil {
		return initState{}, errors.New("system/init cwd is invalid")
	}
	tools, err := decodeClaudeCurrentTools(fields)
	if err != nil {
		return initState{}, err
	}
	mcpCount, err := decodeClaudeCurrentMCPServers(fields)
	if err != nil {
		return initState{}, err
	}
	model := ""
	if rawModel, present, modelErr := claudeCurrentOptionalString(fields, "model", maxModelNameBytes); modelErr != nil {
		return initState{}, errors.New("system/init model is invalid")
	} else if present {
		if !validModelName(rawModel) {
			return initState{}, errors.New("system/init model is invalid")
		}
		model = rawModel
	}
	return initState{sessionID: sessionID, version: version, cwd: cwd, permission: permission, apiKeySource: apiKeySource, model: model, tools: tools, mcpServerCount: mcpCount}, nil
}

func decodeClaudeCurrentTools(fields map[string]providerjson.Value) ([]string, error) {
	value, ok := fields["tools"]
	if !ok || claudeCurrentValueIsNull(value) {
		return nil, errors.New("system/init tools are missing")
	}
	tools := make([]string, 0)
	seen := make(map[string]struct{})
	err := value.Array(func(element providerjson.Value) error {
		if len(tools) >= maxCurrentToolCount {
			return errors.New("system/init tools exceed bounded inventory")
		}
		tool, err := element.String(maxModelNameBytes)
		if err != nil || !validText(tool) {
			return errors.New("system/init tool name is invalid")
		}
		if _, exists := seen[tool]; exists {
			return fmt.Errorf("system/init tool %q is duplicated", tool)
		}
		seen[tool] = struct{}{}
		tools = append(tools, tool)
		return nil
	})
	if err != nil {
		return nil, errors.New("system/init tools are not an array of strings")
	}
	return tools, nil
}

func decodeClaudeCurrentMCPServers(fields map[string]providerjson.Value) (int, error) {
	value, ok := fields["mcp_servers"]
	if !ok || claudeCurrentValueIsNull(value) {
		return 0, errors.New("system/init mcp_servers are missing")
	}
	count := 0
	if err := value.Array(func(element providerjson.Value) error {
		count++
		return element.Validate()
	}); err != nil {
		return 0, errors.New("system/init mcp_servers are not an array")
	}
	return count, nil
}

func applyClaudeCurrentMessageEvent(state *eventState, fields map[string]providerjson.Value, eventType string) {
	if state.resultSeen {
		state.markSemantic(eventType + " event follows result")
		return
	}
	if !state.initSeen {
		state.markSemantic(eventType + " event precedes system/init")
		return
	}
	message, ok := fields["message"]
	if !ok || claudeCurrentValueIsNull(message) {
		return
	}
	if err := validateClaudeCurrentMessage(message, eventType); err != nil {
		state.markSemantic(err.Error())
	}
}

func validateClaudeCurrentMessage(value providerjson.Value, eventType string) error {
	message, err := value.Object("role", "content")
	if err != nil {
		return fmt.Errorf("%s message is not an object: %w", eventType, err)
	}
	if role, present, roleErr := claudeCurrentOptionalString(message, "role", 64); roleErr != nil || (present && !validText(role)) {
		return fmt.Errorf("%s message role is invalid", eventType)
	}
	content, present := message["content"]
	if !present || claudeCurrentValueIsNull(content) {
		return nil
	}
	if err := content.Array(func(block providerjson.Value) error {
		return validateClaudeCurrentContentBlock(block, eventType)
	}); err != nil {
		return err
	}
	return nil
}

func validateClaudeCurrentContentBlock(value providerjson.Value, eventType string) error {
	fields, err := value.Object("type")
	if err != nil {
		return fmt.Errorf("%s message content block is not an object", eventType)
	}
	kind, err := claudeCurrentRequiredText(fields, "type", 64)
	if err != nil {
		return fmt.Errorf("%s message content block type is invalid", eventType)
	}
	switch kind {
	case "text":
		return claudeCurrentStringBlock(value, eventType, "text")
	case "thinking":
		return claudeCurrentStringBlock(value, eventType, "thinking")
	case "redacted_thinking":
		return validateClaudeCurrentRedactedBlock(value, eventType)
	case "tool_use":
		return validateClaudeCurrentToolUse(value, eventType)
	case "tool_result":
		return validateClaudeCurrentToolResult(value, eventType)
	default:
		return fmt.Errorf("unknown %s message content block type %q", eventType, kind)
	}
}

func claudeCurrentStringBlock(value providerjson.Value, eventType, name string) error {
	fields, err := value.Object(name)
	if err != nil {
		return fmt.Errorf("%s %s block: %w", eventType, name, err)
	}
	text, ok := fields[name]
	if !ok {
		return fmt.Errorf("%s %s block: %s is missing", eventType, name, name)
	}
	summary, err := summarizeClaudeValue(text, false)
	if err != nil || summary.invalidNUL {
		return fmt.Errorf("%s %s block: %s is invalid", eventType, name, name)
	}
	return nil
}

func validateClaudeCurrentRedactedBlock(value providerjson.Value, eventType string) error {
	fields, err := value.Object("data", "text", "thinking")
	if err != nil {
		return fmt.Errorf("%s redacted_thinking block: %w", eventType, err)
	}
	for _, name := range []string{"data", "text", "thinking"} {
		if raw, present := fields[name]; present {
			summary, summaryErr := summarizeClaudeValue(raw, false)
			if summaryErr != nil || summary.invalidNUL {
				return fmt.Errorf("%s redacted_thinking block has invalid %s", eventType, name)
			}
			return nil
		}
	}
	return fmt.Errorf("%s redacted_thinking block has no data", eventType)
}

func validateClaudeCurrentToolUse(value providerjson.Value, eventType string) error {
	fields, err := value.Object("id", "name", "input")
	if err != nil {
		return fmt.Errorf("%s tool_use: %w", eventType, err)
	}
	if id, err := claudeCurrentRequiredText(fields, "id", 1024); err != nil || !validText(id) {
		return fmt.Errorf("%s tool_use id is invalid", eventType)
	}
	if name, err := claudeCurrentRequiredText(fields, "name", 256); err != nil || !validText(name) {
		return fmt.Errorf("%s tool_use name is invalid", eventType)
	}
	input, ok := fields["input"]
	if !ok || claudeCurrentValueIsNull(input) {
		return fmt.Errorf("%s tool_use input is not an object", eventType)
	}
	if _, err := input.Object(); err != nil {
		return fmt.Errorf("%s tool_use input: %w", eventType, err)
	}
	return nil
}

func validateClaudeCurrentToolResult(value providerjson.Value, eventType string) error {
	fields, err := value.Object("tool_use_id", "is_error", "content")
	if err != nil {
		return fmt.Errorf("%s tool_result: %w", eventType, err)
	}
	if _, err := claudeCurrentRequiredText(fields, "tool_use_id", 1024); err != nil {
		return fmt.Errorf("%s tool_result id is invalid", eventType)
	}
	if rawError, present := fields["is_error"]; present {
		if _, err := claudeCurrentRequiredBool(fields, "is_error"); err != nil || claudeCurrentValueIsNull(rawError) {
			return fmt.Errorf("%s tool_result is_error is not a boolean", eventType)
		}
	}
	if content, present := fields["content"]; present {
		if err := content.Validate(); err != nil {
			return fmt.Errorf("%s tool_result content has an invalid shape", eventType)
		}
	}
	return nil
}

func applyClaudeCurrentResult(state *eventState, fields map[string]providerjson.Value, value providerjson.Value) {
	if state.resultSeen {
		state.markSemantic("multiple result events")
		return
	}
	if !state.initSeen {
		state.markSemantic("result precedes system/init")
		return
	}
	result, err := decodeClaudeCurrentResult(fields, state.init.sessionID, value)
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	state.resultSeen = true
	state.result = result
}

func decodeClaudeCurrentResult(fields map[string]providerjson.Value, initSessionID string, value providerjson.Value) (resultState, error) {
	subtype, isError, sessionID, err := decodeClaudeCurrentResultIdentity(fields, initSessionID)
	if err != nil {
		return resultState{}, err
	}
	result := resultState{sessionID: sessionID, subtype: subtype, isError: isError, isErrorSeen: true}
	if answerErr := applyClaudeCurrentResultAnswer(&result, fields); answerErr != nil {
		return resultState{}, answerErr
	}
	if stopErr := applyClaudeCurrentResultStopReason(&result, fields); stopErr != nil {
		return resultState{}, stopErr
	}
	diagnostic, errorsPresent, err := decodeClaudeCurrentDiagnostics(fields)
	if err != nil {
		return resultState{}, err
	}
	result.diagnostic = diagnostic
	result.errorsPresent = errorsPresent
	if err := validateClaudeCurrentPermissionDenials(fields); err != nil {
		return resultState{}, err
	}
	applyClaudeCurrentResultUsage(&result, value)
	return result, nil
}

func decodeClaudeCurrentResultIdentity(fields map[string]providerjson.Value, initSessionID string) (string, bool, string, error) {
	subtype, err := claudeCurrentRequiredText(fields, "subtype", 128)
	if err != nil {
		return "", false, "", errors.New("result subtype is missing or not a string")
	}
	if _, known := knownResultSubtypes[subtype]; !known {
		return "", false, "", errors.New("unknown result subtype: " + subtype)
	}
	isError, err := claudeCurrentRequiredBool(fields, "is_error")
	if err != nil {
		return "", false, "", errors.New("result is_error is missing or not a boolean")
	}
	sessionID, err := claudeCurrentRequiredString(fields, "session_id", 128)
	if err != nil || !validSessionID(sessionID) || sessionID != initSessionID {
		return "", false, "", errors.New("result session identity conflicts with system/init")
	}
	if flagErr := validateResultFlags(subtype, isError); flagErr != nil {
		return "", false, "", flagErr
	}
	return subtype, isError, sessionID, nil
}

func applyClaudeCurrentResultAnswer(result *resultState, fields map[string]providerjson.Value) error {
	if answer, present := fields["result"]; present {
		if claudeCurrentValueIsNull(answer) {
			return errors.New("result answer is not a string")
		}
		summary, summaryErr := summarizeClaudeValue(answer, true)
		if summaryErr != nil || summary.invalidNUL {
			return errors.New("result answer is not a string")
		}
		result.answerSeen = true
		result.currentAnswer = summary
		result.currentAnswerSet = true
	}
	return nil
}

func applyClaudeCurrentResultStopReason(result *resultState, fields map[string]providerjson.Value) error {
	if stop, present := fields["stop_reason"]; present {
		if claudeCurrentValueIsNull(stop) {
			result.stopReasonSeen = true
			result.stopReasonNull = true
		} else {
			stopReason, stringErr := stop.String(128)
			if stringErr != nil {
				return errors.New("result stop_reason is not a string or null")
			}
			result.stopReasonSeen = true
			result.stopReason = stopReason
		}
	}
	return nil
}

func applyClaudeCurrentResultUsage(result *resultState, value providerjson.Value) {
	usageFields, usageProblem := selectClaudeCurrentUsageFields(value)
	metadata := parseClaudeCurrentUsage(usageFields)
	if usageProblem {
		metadata.usagePresent = true
		metadata.usageReliable = false
	}
	result.usage = metadata.usage
	result.totalCostUSD = metadata.totalCostUSD
	result.durationMS = metadata.durationMS
	result.numTurns = metadata.numTurns
	result.currentUsagePresent = metadata.usagePresent
	result.currentUsageReliable = metadata.usageReliable
}

func selectClaudeCurrentUsageFields(value providerjson.Value) (map[string]providerjson.Value, bool) {
	names := []string{"usage", "modelUsage", "total_cost_usd", "duration_ms", "num_turns"}
	fields, err := value.Object(names...)
	if err == nil {
		return fields, false
	}
	// Optional accounting is advisory. Isolate each member so a duplicate
	// optional field does not hide valid siblings or reject the answer.
	fields = make(map[string]providerjson.Value, len(names))
	problem := false
	for _, name := range names {
		selected, selectedErr := value.Object(name)
		if selectedErr != nil {
			problem = true
			continue
		}
		if field, present := selected[name]; present {
			fields[name] = field
		}
	}
	return fields, problem
}

func decodeClaudeCurrentDiagnostics(fields map[string]providerjson.Value) (string, bool, error) {
	value, ok := fields["errors"]
	if !ok {
		return "", false, nil
	}
	if claudeCurrentValueIsNull(value) {
		return "", false, errors.New("result errors must be an array of strings")
	}
	first := ""
	count := 0
	if err := value.Array(func(element providerjson.Value) error {
		count++
		text, err := element.String(maxEventLineBytes)
		if err != nil || !validDiagnostic(text) {
			return errors.New("result error diagnostic is invalid")
		}
		if first == "" && strings.TrimSpace(text) != "" {
			first = text
		}
		return nil
	}); err != nil {
		return "", false, errors.New("result errors are not an array of strings")
	}
	return first, count != 0, nil
}

func validateClaudeCurrentPermissionDenials(fields map[string]providerjson.Value) error {
	value, ok := fields["permission_denials"]
	if !ok {
		return nil
	}
	if claudeCurrentValueIsNull(value) {
		return errors.New("result permission_denials must be an array")
	}
	if err := value.Array(func(element providerjson.Value) error {
		if claudeCurrentValueIsNull(element) {
			return errors.New("result permission_denials entry is not an object")
		}
		_, err := element.Object()
		if err != nil {
			return errors.New("result permission_denials entry is not an object")
		}
		return nil
	}); err != nil {
		return errors.New("result permission_denials are not an array")
	}
	return nil
}

// parseClaudeCurrentUsage treats provider accounting as advisory. A malformed
// optional usage object must not turn an otherwise valid answer into a native
// failure, and unknown provider counters are intentionally not synthesized.
func parseClaudeCurrentUsage(fields map[string]providerjson.Value) resultUsage {
	metadata := resultUsage{usageReliable: true}
	if usageValue, present := fields["usage"]; present {
		metadata.usagePresent = true
		metadata.usage, metadata.usageReliable = parseClaudeCurrentTokenUsage(usageValue)
	}
	if value, present := fields["modelUsage"]; present {
		// The current parser deliberately does not retain arbitrary model-keyed
		// envelopes. Validate their JSON shape, but do not flatten or invent
		// per-model totals. A malformed optional envelope is warned about below.
		metadata.usagePresent = true
		if err := value.Validate(); err != nil {
			metadata.usageReliable = false
		}
	}
	for _, name := range []string{"total_cost_usd", "duration_ms", "num_turns"} {
		applyClaudeCurrentScalarUsage(&metadata, fields, name)
	}
	return metadata
}

func parseClaudeCurrentTokenUsage(value providerjson.Value) (*usageEnvelope, bool) {
	usage := &usageEnvelope{}
	if claudeCurrentValueIsNull(value) {
		return usage, false
	}
	fields, err := value.Object("input_tokens", "output_tokens", "thinking_tokens", "total_tokens", "cache_read_input_tokens", "cache_creation_input_tokens")
	if err != nil {
		return usage, false
	}
	reliable := true
	known := 0
	usage.input, reliable = claudeCurrentUsageCounter(fields, "input_tokens", reliable, &known)
	usage.output, reliable = claudeCurrentUsageCounter(fields, "output_tokens", reliable, &known)
	usage.thinking, reliable = claudeCurrentUsageCounter(fields, "thinking_tokens", reliable, &known)
	usage.total, reliable = claudeCurrentUsageCounter(fields, "total_tokens", reliable, &known)
	usage.cacheRead, reliable = claudeCurrentUsageCounter(fields, "cache_read_input_tokens", reliable, &known)
	usage.cacheCreation, reliable = claudeCurrentUsageCounter(fields, "cache_creation_input_tokens", reliable, &known)
	return usage, reliable && known > 0
}

func applyClaudeCurrentScalarUsage(metadata *resultUsage, fields map[string]providerjson.Value, name string) {
	value, present := fields[name]
	if !present {
		return
	}
	metadata.usagePresent = true
	if claudeCurrentValueIsNull(value) {
		metadata.usageReliable = false
		return
	}
	switch name {
	case "total_cost_usd":
		decoded, err := claudeCurrentOptionalDecimal(fields, name)
		if err != nil {
			metadata.usageReliable = false
			return
		}
		metadata.totalCostUSD = decoded
	case "duration_ms":
		decoded, err := claudeCurrentOptionalNonnegativeInt(fields, name)
		if err != nil {
			metadata.usageReliable = false
			return
		}
		metadata.durationMS = decoded
	case "num_turns":
		decoded, err := claudeCurrentOptionalNonnegativeInt(fields, name)
		if err != nil {
			metadata.usageReliable = false
			return
		}
		metadata.numTurns = decoded
	}
}

func claudeCurrentUsageCounter(fields map[string]providerjson.Value, name string, reliable bool, known *int) (*int64, bool) {
	value, present := fields[name]
	if !present {
		return nil, reliable
	}
	*known++
	if claudeCurrentValueIsNull(value) {
		return nil, false
	}
	decoded, err := claudeCurrentOptionalNonnegativeInt(fields, name)
	if err != nil {
		return nil, false
	}
	return decoded, reliable
}
