package claude

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
	resultSuccess                         = "success"
	resultErrorDuringExecution            = "error_during_execution"
	resultErrorMaxTurns                   = "error_max_turns"
	resultErrorMaxBudget                  = "error_max_budget_usd"
	resultErrorMaxStructuredOutputRetries = "error_max_structured_output_retries"
)

var knownResultSubtypes = map[string]struct{}{
	resultSuccess:                         {},
	resultErrorDuringExecution:            {},
	resultErrorMaxTurns:                   {},
	resultErrorMaxBudget:                  {},
	resultErrorMaxStructuredOutputRetries: {},
}

type eventState struct {
	mode             string
	strict           bool
	nativeHistorical bool
	initSeen         bool
	init             initState

	// observedSessionID binds identities on every event, including events that
	// arrive before system/init.  A later init cannot make an earlier
	// conflicting session identity disappear.
	observedSessionID string

	resultSeen bool
	result     resultState

	semanticErr error
}

type initState struct {
	sessionID      string
	version        string
	cwd            string
	permission     string
	apiKeySource   string
	model          string
	tools          []string
	mcpServerCount int
}

type resultState struct {
	sessionID      string
	subtype        string
	isError        bool
	isErrorSeen    bool
	answer         string
	answerSeen     bool
	stopReason     string
	stopReasonSeen bool
	stopReasonNull bool
	diagnostic     string
	errorsPresent  bool
	usage          *usageEnvelope
	modelUsage     []modelUsageRecord
	totalCostUSD   *string
	durationMS     *int64
	numTurns       *int64
}

// parsedEvent deliberately retains only the top-level object fields needed by
// the state machine. Unknown event payloads are discarded after structural and
// duplicate-key validation, so provider text cannot accumulate in parser state.
type parsedEvent struct {
	typeName string
	fields   map[string]json.RawMessage
}

func (s *eventState) markSemantic(reason string) {
	if s.semanticErr == nil {
		s.semanticErr = fmt.Errorf("invalid Claude stream-json evidence: %s", reason)
	}
}

func (s eventState) semanticFault() bool { return s.semanticErr != nil }

// parseStdout consumes every byte supplied by the capture reader. Semantic
// faults stop state growth but never stop reads, preserving the core's ability
// to reach EOF and seal a finite provider invocation. Only one bounded line and
// completion-relevant scalar fields are retained by the shared framer and this
// provider callback.
func parseStdout(reader io.Reader) (eventState, error) {
	return parseStdoutForMode(reader, Mode)
}

func parseStdoutForMode(reader io.Reader, mode string) (eventState, error) {
	return parseStdoutForModeWithPolicy(reader, mode, false)
}

func parseLegacyStdoutForMode(reader io.Reader, mode string) (eventState, error) {
	return parseStdoutForModeWithPolicy(reader, mode, true)
}

func parseHistoricalNativeStdoutForMode(reader io.Reader, mode string) (eventState, error) {
	return parseStdoutForModeWithOptions(reader, mode, false, true)
}

func parseStdoutForModeWithPolicy(reader io.Reader, mode string, strict bool) (eventState, error) {
	return parseStdoutForModeWithOptions(reader, mode, strict, false)
}

func parseStdoutForModeWithOptions(reader io.Reader, mode string, strict, nativeHistorical bool) (eventState, error) {
	if mode != Mode && mode != WorkspaceWriteMode {
		return eventState{mode: mode, strict: strict, nativeHistorical: nativeHistorical, semanticErr: fmt.Errorf("unsupported Claude permission mode %q", mode)}, nil
	}
	state := eventState{mode: mode, strict: strict, nativeHistorical: nativeHistorical}
	semanticErr, readErr := commonprovider.ReadJSONL(reader, maxEventLineBytes, func(line []byte) error {
		return processEventLine(&state, line)
	})
	if state.semanticErr == nil && semanticErr != nil {
		state.markSemantic(semanticErr.Error())
	}
	if readErr != nil {
		return state, fmt.Errorf("reading Claude stdout: %w", readErr)
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
		return parsedEvent{}, fmt.Errorf("decoding event object: %w", err)
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
	if !bindEventSession(state, event.typeName, event.fields) {
		return
	}
	switch event.typeName {
	case "system":
		applySystemEvent(state, event.fields)
	case "result":
		applyResultEvent(state, event.fields)
	case "assistant":
		applyAssistantEvent(state, event.fields)
	case "user":
		applyUserEvent(state, event.fields)
	case "error":
		state.markSemantic("top-level error event is terminally ambiguous")
	default:
		if strings.HasPrefix(event.typeName, "thread.") || strings.HasPrefix(event.typeName, "turn.") || strings.HasPrefix(event.typeName, "session.") {
			state.markSemantic("unknown thread/turn/session lifecycle event: " + event.typeName)
		}
	}
}

func bindEventSession(state *eventState, eventType string, fields map[string]json.RawMessage) bool {
	rawSessionID, exists := fields["session_id"]
	if !exists {
		return true
	}
	sessionID, err := decodeString(rawSessionID)
	if err != nil || !validSessionID(sessionID) {
		state.markSemantic(eventType + " session identity is invalid")
		return false
	}
	if state.observedSessionID != "" && sessionID != state.observedSessionID {
		state.markSemantic(eventType + " session identity changed")
		return false
	}
	if state.initSeen && sessionID != state.init.sessionID {
		state.markSemantic(eventType + " session identity conflicts with system/init")
		return false
	}
	state.observedSessionID = sessionID
	return true
}

func applySystemEvent(state *eventState, fields map[string]json.RawMessage) {
	if rawSessionID, exists := fields["session_id"]; exists {
		sessionID, err := decodeString(rawSessionID)
		if err != nil || !validSessionID(sessionID) || state.initSeen && sessionID != state.init.sessionID {
			state.markSemantic("system event session identity conflicts with system/init")
			return
		}
	}
	subtype, present, valid := readOptionalString(fields, "subtype")
	if !valid {
		state.markSemantic("system subtype is not a string")
		return
	}
	if !present || subtype != "init" {
		if ambiguousSystemSubtype(subtype) {
			state.markSemantic("unknown terminal system lifecycle event: " + subtype)
		}
		return
	}
	applyInitEvent(state, fields)
}

func applyInitEvent(state *eventState, fields map[string]json.RawMessage) {
	if state.initSeen {
		state.markSemantic("multiple system/init events")
		return
	}
	init, err := decodeInitForModeWithOptions(fields, state.mode, state.strict, state.nativeHistorical)
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	state.initSeen = true
	state.init = init
}

func decodeInit(fields map[string]json.RawMessage) (initState, error) {
	return decodeInitForMode(fields, Mode)
}

func decodeInitForMode(fields map[string]json.RawMessage, mode string) (initState, error) {
	return decodeInitForModeWithPolicy(fields, mode, true)
}

func decodeInitForModeWithPolicy(fields map[string]json.RawMessage, mode string, strict bool) (initState, error) {
	return decodeInitForModeWithOptions(fields, mode, strict, false)
}

func decodeInitForModeWithOptions(fields map[string]json.RawMessage, mode string, strict, nativeHistorical bool) (initState, error) {
	sessionID, version, err := decodeInitIdentity(fields)
	if err != nil {
		return initState{}, err
	}
	permission, apiKeySource, err := decodeInitPolicyForModeWithOptions(fields, mode, strict, nativeHistorical)
	if err != nil {
		return initState{}, err
	}
	cwd, err := decodeInitCWD(fields)
	if err != nil {
		return initState{}, err
	}
	tools, err := allowedToolsForModeWithPolicy(fields, mode, strict)
	if err != nil {
		return initState{}, err
	}
	mcpCount, err := mcpServersWithPolicy(fields, strict)
	if err != nil {
		return initState{}, err
	}
	model, err := decodeInitModel(fields)
	if err != nil {
		return initState{}, err
	}
	return initState{sessionID: sessionID, version: version, cwd: cwd, permission: permission, apiKeySource: apiKeySource, model: model, tools: tools, mcpServerCount: mcpCount}, nil
}

func decodeInitIdentity(fields map[string]json.RawMessage) (string, string, error) {
	sessionID, err := requiredString(fields, "session_id")
	if err != nil || !validSessionID(sessionID) {
		return "", "", errors.New("system/init session_id is not a UUID")
	}
	version, err := requiredString(fields, "claude_code_version")
	if err != nil || !validText(version) {
		return "", "", errors.New("system/init claude_code_version is invalid")
	}
	return sessionID, version, nil
}

func decodeInitPolicy(fields map[string]json.RawMessage) (string, string, error) {
	return decodeInitPolicyForMode(fields, Mode)
}

func decodeInitPolicyForMode(fields map[string]json.RawMessage, mode string) (string, string, error) {
	return decodeInitPolicyForModeWithPolicy(fields, mode, true)
}

func decodeInitPolicyForModeWithPolicy(fields map[string]json.RawMessage, mode string, strict bool) (string, string, error) {
	return decodeInitPolicyForModeWithOptions(fields, mode, strict, false)
}

func decodeInitPolicyForModeWithOptions(fields map[string]json.RawMessage, mode string, strict, nativeHistorical bool) (string, string, error) {
	permission, err := requiredString(fields, "permissionMode")
	var wantPermission string
	switch mode {
	case Mode:
		wantPermission = "plan"
		if strict || nativeHistorical {
			wantPermission = "dontAsk"
		}
	case WorkspaceWriteMode:
		wantPermission = "acceptEdits"
	default:
		return "", "", fmt.Errorf("unsupported Claude permission mode %q", mode)
	}
	if err != nil || permission != wantPermission {
		return "", "", fmt.Errorf("system/init permissionMode is not %s", wantPermission)
	}
	if strict {
		apiKeySource, policyErr := requiredString(fields, "apiKeySource")
		if policyErr != nil || apiKeySource != "none" {
			return "", "", errors.New("system/init apiKeySource is not none")
		}
		return permission, apiKeySource, nil
	}
	apiKeySource, err := requiredString(fields, "apiKeySource")
	if err != nil || !validText(apiKeySource) {
		return "", "", errors.New("system/init apiKeySource is invalid")
	}
	return permission, apiKeySource, nil
}

func decodeInitCWD(fields map[string]json.RawMessage) (string, error) {
	cwd, err := requiredString(fields, "cwd")
	if err != nil || !validText(cwd) {
		return "", errors.New("system/init cwd is invalid")
	}
	return cwd, nil
}

func decodeInitModel(fields map[string]json.RawMessage) (string, error) {
	rawModel, exists := fields["model"]
	if !exists {
		return "", nil
	}
	model, err := decodeString(rawModel)
	if err != nil || !validModelName(model) {
		return "", errors.New("system/init model is invalid")
	}
	return model, nil
}

func allowedTools(fields map[string]json.RawMessage) ([]string, error) {
	return allowedToolsForMode(fields, Mode)
}

func allowedToolsForMode(fields map[string]json.RawMessage, mode string) ([]string, error) {
	return allowedToolsForModeWithPolicy(fields, mode, true)
}

func allowedToolsForModeWithPolicy(fields map[string]json.RawMessage, mode string, strict bool) ([]string, error) {
	tools, err := decodeToolList(fields)
	if err != nil {
		return nil, err
	}
	if !strict {
		return validateNativeToolList(tools)
	}
	return validateLegacyToolList(tools, mode)
}

func decodeToolList(fields map[string]json.RawMessage) ([]string, error) {
	raw, ok := fields["tools"]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, errors.New("system/init tools are missing")
	}
	var tools []string
	if err := json.Unmarshal(raw, &tools); err != nil || tools == nil {
		return nil, errors.New("system/init tools are not an array of strings")
	}
	return tools, nil
}

func validateNativeToolList(tools []string) ([]string, error) {
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if !validText(tool) {
			return nil, errors.New("system/init tool name is invalid")
		}
		if _, exists := seen[tool]; exists {
			return nil, fmt.Errorf("system/init tool %q is duplicated", tool)
		}
		seen[tool] = struct{}{}
	}
	return tools, nil
}

func validateLegacyToolList(tools []string, mode string) ([]string, error) {
	seen := make(map[string]struct{}, len(tools))
	required := map[string]bool{"Read": false, "Glob": false, "Grep": false}
	if mode == WorkspaceWriteMode {
		required["Edit"] = false
		required["Write"] = false
	}
	for _, tool := range tools {
		if !validText(tool) {
			return nil, errors.New("system/init tool name is invalid")
		}
		if _, exists := seen[tool]; exists {
			return nil, fmt.Errorf("system/init tool %q is duplicated", tool)
		}
		seen[tool] = struct{}{}
		if _, okay := required[tool]; !okay && tool != "EndConversation" {
			return nil, fmt.Errorf("system/init exposes unsupported tool %q", tool)
		}
		required[tool] = true
	}
	for tool, found := range required {
		if !found {
			return nil, fmt.Errorf("system/init is missing required tool %q", tool)
		}
	}
	return tools, nil
}

func emptyMCPServers(fields map[string]json.RawMessage) (int, error) {
	return mcpServersWithPolicy(fields, true)
}

func mcpServersWithPolicy(fields map[string]json.RawMessage, strict bool) (int, error) {
	raw, ok := fields["mcp_servers"]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, errors.New("system/init mcp_servers are missing")
	}
	var servers []json.RawMessage
	if err := json.Unmarshal(raw, &servers); err != nil || servers == nil {
		return 0, errors.New("system/init mcp_servers are not an array")
	}
	if strict && len(servers) != 0 {
		return len(servers), errors.New("system/init exposes MCP servers")
	}
	return len(servers), nil
}

func applyAssistantEvent(state *eventState, fields map[string]json.RawMessage) {
	if state.resultSeen {
		state.markSemantic("assistant event follows result")
		return
	}
	if !state.initSeen {
		state.markSemantic("assistant event precedes system/init")
		return
	}
	if err := validateMessageContentWithPolicy(fields, "assistant", state.mode, state.strict); err != nil {
		state.markSemantic(err.Error())
	}
}

func applyUserEvent(state *eventState, fields map[string]json.RawMessage) {
	if state.resultSeen {
		state.markSemantic("user event follows result")
		return
	}
	if !state.initSeen {
		state.markSemantic("user event precedes system/init")
		return
	}
	if err := validateMessageContentWithPolicy(fields, "user", state.mode, state.strict); err != nil {
		state.markSemantic(err.Error())
	}
}

func validateMessageContent(fields map[string]json.RawMessage, eventType, mode string) error {
	return validateMessageContentWithPolicy(fields, eventType, mode, true)
}

func validateMessageContentWithPolicy(fields map[string]json.RawMessage, eventType, mode string, strict bool) error {
	rawMessage, exists := fields["message"]
	if !exists || bytes.Equal(bytes.TrimSpace(rawMessage), []byte("null")) {
		return nil
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(rawMessage, &message); err != nil || message == nil {
		return fmt.Errorf("%s message is not an object", eventType)
	}
	rawContent, exists := message["content"]
	if !exists || bytes.Equal(bytes.TrimSpace(rawContent), []byte("null")) {
		return nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(rawContent, &blocks); err != nil || blocks == nil {
		return fmt.Errorf("%s message content is not an array", eventType)
	}
	for _, rawBlock := range blocks {
		if err := validateContentBlockWithPolicy(rawBlock, eventType, mode, strict); err != nil {
			return err
		}
	}
	return nil
}

func validateContentBlock(rawBlock json.RawMessage, eventType, mode string) error {
	return validateContentBlockWithPolicy(rawBlock, eventType, mode, true)
}

func validateContentBlockWithPolicy(rawBlock json.RawMessage, eventType, mode string, strict bool) error {
	var block map[string]json.RawMessage
	if err := json.Unmarshal(rawBlock, &block); err != nil || block == nil {
		return fmt.Errorf("%s message content block is not an object", eventType)
	}
	kind, err := requiredString(block, "type")
	if err != nil || kind == "" {
		return fmt.Errorf("%s message content block type is invalid", eventType)
	}
	return validateContentBlockKindWithPolicy(block, kind, eventType, mode, strict)
}

func validateContentBlockKind(block map[string]json.RawMessage, kind, eventType, mode string) error {
	return validateContentBlockKindWithPolicy(block, kind, eventType, mode, true)
}

func validateContentBlockKindWithPolicy(block map[string]json.RawMessage, kind, eventType, mode string, strict bool) error {
	switch kind {
	case "text":
		return validateTextBlock(block, eventType)
	case "thinking":
		return validateThinkingBlock(block, eventType)
	case "redacted_thinking":
		return validateRedactedThinkingBlock(block, eventType)
	case "tool_use":
		return validateToolUseBlockWithPolicy(block, eventType, mode, strict)
	case "tool_result":
		return validateToolResultBlock(block, eventType)
	default:
		return fmt.Errorf("unknown %s message content block type %q", eventType, kind)
	}
}

func validateTextBlock(block map[string]json.RawMessage, eventType string) error {
	if err := requireContentString(block, "text"); err != nil {
		return fmt.Errorf("%s text block: %w", eventType, err)
	}
	return nil
}

func validateThinkingBlock(block map[string]json.RawMessage, eventType string) error {
	if err := requireContentString(block, "thinking"); err != nil {
		return fmt.Errorf("%s thinking block: %w", eventType, err)
	}
	return nil
}

func validateRedactedThinkingBlock(block map[string]json.RawMessage, eventType string) error {
	if hasContentString(block, "data") || hasContentString(block, "text") || hasContentString(block, "thinking") {
		return nil
	}
	return fmt.Errorf("%s redacted thinking block has no data", eventType)
}

func validateToolUseBlock(block map[string]json.RawMessage, eventType, mode string) error {
	return validateToolUseBlockWithPolicy(block, eventType, mode, true)
}

func validateToolUseBlockWithPolicy(block map[string]json.RawMessage, eventType, mode string, strict bool) error {
	id, idErr := requiredString(block, "id")
	name, nameErr := requiredString(block, "name")
	if idErr != nil || !validText(id) {
		return fmt.Errorf("%s tool_use id is invalid", eventType)
	}
	if nameErr != nil || !validText(name) {
		return fmt.Errorf("%s tool_use name is invalid", eventType)
	}
	if strict && !allowedToolNameForMode(name, mode) {
		return fmt.Errorf("%s emitted unsupported tool_use %q", eventType, name)
	}
	if err := requireObject(block, "input"); err != nil {
		return fmt.Errorf("%s tool_use input: %w", eventType, err)
	}
	return nil
}

func validateToolResultBlock(block map[string]json.RawMessage, eventType string) error {
	toolUseID, err := requiredString(block, "tool_use_id")
	if err != nil || !validText(toolUseID) {
		return fmt.Errorf("%s tool_result id is invalid", eventType)
	}
	if err := validateToolResultErrorFlag(block); err != nil {
		return fmt.Errorf("%s tool_result is_error is not a boolean", eventType)
	}
	if rawContent, exists := block["content"]; exists && !validToolResultContent(rawContent) {
		return fmt.Errorf("%s tool_result content has an invalid shape", eventType)
	}
	return nil
}

func validateToolResultErrorFlag(block map[string]json.RawMessage) error {
	rawError, exists := block["is_error"]
	if !exists {
		return nil
	}
	_, err := requiredBool(map[string]json.RawMessage{"is_error": rawError}, "is_error")
	return err
}

func allowedToolName(name string) bool {
	return allowedToolNameForMode(name, Mode)
}

func allowedToolNameForMode(name, mode string) bool {
	switch name {
	case "Read", "Glob", "Grep", "EndConversation":
		return true
	case "Edit", "Write":
		return mode == WorkspaceWriteMode
	default:
		return false
	}
}

func requireContentString(fields map[string]json.RawMessage, name string) error {
	if _, err := requiredString(fields, name); err != nil {
		return errors.New(name + " is not a string")
	}
	return nil
}

func hasContentString(fields map[string]json.RawMessage, name string) bool {
	raw, exists := fields[name]
	if !exists {
		return false
	}
	_, err := decodeString(raw)
	return err == nil
}

func requireObject(fields map[string]json.RawMessage, name string) error {
	raw, exists := fields[name]
	if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New(name + " is not an object")
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return errors.New(name + " is not an object")
	}
	return nil
}

func validToolResultContent(raw json.RawMessage) bool {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return true
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	switch value.(type) {
	case string, []any, map[string]any:
		return true
	default:
		return false
	}
}

func applyResultEvent(state *eventState, fields map[string]json.RawMessage) {
	if state.resultSeen {
		state.markSemantic("multiple result events")
		return
	}
	if !state.initSeen {
		state.markSemantic("result precedes system/init")
		return
	}
	result, err := decodeResult(fields, state.init.sessionID)
	if err != nil {
		state.markSemantic(err.Error())
		return
	}
	state.resultSeen = true
	state.result = result
}

func decodeResult(fields map[string]json.RawMessage, initSessionID string) (resultState, error) {
	subtype, isError, sessionID, err := decodeResultHeader(fields, initSessionID)
	if err != nil {
		return resultState{}, err
	}
	answer, answerSeen, stopReason, stopReasonSeen, stopReasonNull, diagnostic, errorsPresent, err := decodeResultText(fields)
	if err != nil {
		return resultState{}, err
	}
	metadata, err := decodeResultUsage(fields)
	if err != nil {
		return resultState{}, err
	}
	return resultState{
		sessionID: sessionID, subtype: subtype, isError: isError, isErrorSeen: true,
		answer: answer, answerSeen: answerSeen, stopReason: stopReason,
		stopReasonSeen: stopReasonSeen, stopReasonNull: stopReasonNull,
		diagnostic: diagnostic, errorsPresent: errorsPresent, usage: metadata.usage, modelUsage: metadata.modelUsage,
		totalCostUSD: metadata.totalCostUSD, durationMS: metadata.durationMS, numTurns: metadata.numTurns,
	}, nil
}

func decodeResultHeader(fields map[string]json.RawMessage, initSessionID string) (string, bool, string, error) {
	subtype, err := requiredString(fields, "subtype")
	if err != nil {
		return "", false, "", errors.New("result subtype is missing or not a string")
	}
	if _, known := knownResultSubtypes[subtype]; !known {
		return "", false, "", errors.New("unknown result subtype: " + subtype)
	}
	isError, err := requiredBool(fields, "is_error")
	if err != nil {
		return "", false, "", errors.New("result is_error is missing or not a boolean")
	}
	sessionID, err := requiredString(fields, "session_id")
	if err != nil || !validSessionID(sessionID) || sessionID != initSessionID {
		return "", false, "", errors.New("result session identity conflicts with system/init")
	}
	if err := validateResultFlags(subtype, isError); err != nil {
		return "", false, "", err
	}
	return subtype, isError, sessionID, nil
}

func validateResultFlags(subtype string, isError bool) error {
	if subtype == resultSuccess && isError {
		return errors.New("successful result has is_error=true")
	}
	if subtype != resultSuccess && !isError {
		return errors.New("error result has is_error=false")
	}
	return nil
}

func decodeResultText(fields map[string]json.RawMessage) (string, bool, string, bool, bool, string, bool, error) {
	answer, answerSeen, err := optionalStringField(fields, "result")
	if err != nil {
		return "", false, "", false, false, "", false, errors.New("result answer is not a string")
	}
	stopReason, stopReasonSeen, stopReasonNull, err := optionalStringOrNull(fields, "stop_reason")
	if err != nil {
		return "", false, "", false, false, "", false, errors.New("result stop_reason is not a string or null")
	}
	diagnostic, errorsPresent, err := resultDiagnostic(fields)
	if err != nil {
		return "", false, "", false, false, "", false, err
	}
	return answer, answerSeen, stopReason, stopReasonSeen, stopReasonNull, diagnostic, errorsPresent, nil
}

type resultUsage struct {
	usage        *usageEnvelope
	modelUsage   []modelUsageRecord
	totalCostUSD *string
	durationMS   *int64
	numTurns     *int64
}

func decodeResultUsage(fields map[string]json.RawMessage) (resultUsage, error) {
	usage, err := parseUsageEnvelope(fields)
	if err != nil {
		return resultUsage{}, err
	}
	modelUsage, err := parseModelUsage(fields)
	if err != nil {
		return resultUsage{}, err
	}
	totalCostUSD, err := optionalDecimalField(fields, "total_cost_usd")
	if err != nil {
		return resultUsage{}, err
	}
	durationMS, err := optionalNonnegativeIntField(fields, "duration_ms")
	if err != nil {
		return resultUsage{}, err
	}
	numTurns, err := optionalNonnegativeIntField(fields, "num_turns")
	if err != nil {
		return resultUsage{}, err
	}
	if err := validatePermissionDenials(fields); err != nil {
		return resultUsage{}, err
	}
	return resultUsage{usage: usage, modelUsage: modelUsage, totalCostUSD: totalCostUSD, durationMS: durationMS, numTurns: numTurns}, nil
}

func resultDiagnostic(fields map[string]json.RawMessage) (string, bool, error) {
	raw, ok := fields["errors"]
	if !ok {
		return "", false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false, errors.New("result errors must be an array of strings")
	}
	var diagnostics []json.RawMessage
	if err := json.Unmarshal(raw, &diagnostics); err != nil || diagnostics == nil {
		return "", false, errors.New("result errors are not an array of strings")
	}
	firstDiagnostic := ""
	for _, rawDiagnostic := range diagnostics {
		diagnostic, err := decodeString(rawDiagnostic)
		if err != nil {
			return "", false, errors.New("result error diagnostic is invalid")
		}
		if !validDiagnostic(diagnostic) {
			return "", false, errors.New("result error diagnostic is invalid")
		}
		if firstDiagnostic == "" && strings.TrimSpace(diagnostic) != "" {
			firstDiagnostic = diagnostic
		}
	}
	return firstDiagnostic, len(diagnostics) != 0, nil
}

func validatePermissionDenials(fields map[string]json.RawMessage) error {
	raw, ok := fields["permission_denials"]
	if !ok {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("result permission_denials must be an array")
	}
	var denials []json.RawMessage
	if err := json.Unmarshal(raw, &denials); err != nil || denials == nil {
		return errors.New("result permission_denials are not an array")
	}
	for _, rawDenial := range denials {
		var denial map[string]json.RawMessage
		if err := json.Unmarshal(rawDenial, &denial); err != nil || denial == nil {
			return errors.New("result permission_denials entry is not an object")
		}
	}
	return nil
}

func ambiguousSystemSubtype(subtype string) bool {
	if subtype == "" {
		return false
	}
	lower := strings.ToLower(subtype)
	for _, marker := range []string{"abort", "cancel", "error", "fail", "interrupt", "shutdown", "stop", "timeout", "timed_out", "timed-out", "refusal"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func validDiagnostic(value string) bool {
	return len(value) <= maxEventLineBytes && !strings.ContainsRune(value, '\x00') && utf8.ValidString(value)
}

func requiredString(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok {
		return "", fmt.Errorf("missing %s", name)
	}
	return decodeString(raw)
}

func optionalString(fields map[string]json.RawMessage, name string) (string, bool) {
	value, present, valid := readOptionalString(fields, name)
	return value, present && valid
}

func readOptionalString(fields map[string]json.RawMessage, name string) (string, bool, bool) {
	raw, ok := fields[name]
	if !ok {
		return "", false, true
	}
	value, err := decodeString(raw)
	if err != nil {
		return "", true, false
	}
	return value, true, true
}

func optionalStringField(fields map[string]json.RawMessage, name string) (string, bool, error) {
	raw, ok := fields[name]
	if !ok {
		return "", false, nil
	}
	value, err := decodeString(raw)
	if err != nil {
		return "", true, err
	}
	return value, true, nil
}

func optionalStringOrNull(fields map[string]json.RawMessage, name string) (string, bool, bool, error) {
	raw, ok := fields[name]
	if !ok {
		return "", false, false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", true, true, nil
	}
	value, err := decodeString(raw)
	if err != nil {
		return "", true, false, err
	}
	return value, true, false, nil
}

func requiredBool(fields map[string]json.RawMessage, name string) (bool, error) {
	raw, ok := fields[name]
	if !ok {
		return false, fmt.Errorf("missing %s", name)
	}
	trimmed := bytes.TrimSpace(raw)
	if !bytes.Equal(trimmed, []byte("true")) && !bytes.Equal(trimmed, []byte("false")) {
		return false, errors.New("value is not a JSON boolean")
	}
	var value bool
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return false, err
	}
	return value, nil
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

func validText(value string) bool {
	return value != "" && strings.TrimSpace(value) != "" && !strings.ContainsRune(value, '\x00') && utf8.ValidString(value)
}
