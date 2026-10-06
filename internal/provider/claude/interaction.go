package claude

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const claudeInteractionInitialize = "delegate-initialize"

var _ execution.CurrentRecordInteractionProtocol = (*claudeInteraction)(nil)

type claudeQuestionInput struct {
	Questions []struct {
		Question    string `json:"question"`
		MultiSelect bool   `json:"multiSelect"`
		Options     []struct {
			Label string `json:"label"`
		} `json:"options"`
	} `json:"questions"`
}

type claudePendingQuestion struct {
	nativeID  string
	toolUseID string
	messageID string
	input     map[string]json.RawMessage
	questions []task.InteractionQuestion
}

type claudeInteraction struct {
	turnID        string
	expected      string
	record        func(task.SessionIdentity) error
	state         eventState
	started       bool
	initialized   bool
	finished      bool
	brief         string
	sessionState  string
	resultSeen    bool
	failed        bool
	pending       map[string]claudePendingQuestion
	agents        map[string]bool
	recordCapture bool
}

func newClaudeInteraction(request task.TaskRecord, expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.InteractionProtocol, error) {
	return newClaudeInteractionWithCapture(request, expected, record, true)
}

func newClaudeInteractionWithCapture(request task.TaskRecord, expected task.SessionExpectation, record func(task.SessionIdentity) error, recordCapture bool) (execution.InteractionProtocol, error) {
	id, err := expectedSessionID(request.RootID, request.TaskID, expected)
	if err != nil {
		return nil, err
	}
	return &claudeInteraction{turnID: request.TaskID, expected: id, record: record, state: eventState{mode: request.Mode, current: recordCapture}, pending: map[string]claudePendingQuestion{}, agents: map[string]bool{}, recordCapture: recordCapture}, nil
}

func (p *claudeInteraction) UseRecordCapture() bool { return p.recordCapture }

func (p *claudeInteraction) Start(brief []byte) ([][]byte, error) {
	if p.started || !utf8.Valid(brief) || len(brief) == 0 {
		return nil, task.ErrInteractionRequest
	}
	p.started = true
	p.brief = string(brief)
	frame, err := json.Marshal(map[string]any{"type": "control_request", "request_id": claudeInteractionInitialize, "request": map[string]string{"subtype": "initialize"}})
	return [][]byte{frame}, err
}

func (p *claudeInteraction) Receive(line []byte) (execution.InteractionUpdate, error) {
	if !utf8.Valid(line) {
		return execution.InteractionUpdate{}, task.ErrEvidenceFault
	}
	event, err := decodeEvent(line)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	switch event.typeName {
	case "control_response":
		return p.initializeResponse(line)
	case "control_request":
		return p.controlRequest(line)
	case "control_cancel_request":
		return p.cancelRequest(event.fields)
	default:
		return p.observeEvent(event)
	}
}

// ReceiveRecord consumes one callback-scoped Claude record without retaining
// the record bytes. It is the current interactive protocol path; Receive
// remains the historical line/framing compatibility path.
func (p *claudeInteraction) ReceiveRecord(value providerjson.Value) (execution.InteractionUpdate, error) {
	event, err := decodeClaudeCurrentEvent(value)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	switch event.typeName {
	case "control_response":
		return p.initializeResponseRecord(event.fields)
	case "control_request":
		return p.controlRequestRecord(event.fields)
	case "control_cancel_request":
		return p.cancelRequestRecord(event.fields)
	default:
		return p.observeCurrentEvent(event)
	}
}

func (p *claudeInteraction) initializeResponseRecord(fields map[string]providerjson.Value) (execution.InteractionUpdate, error) {
	response, ok := fields["response"]
	if !ok {
		return execution.InteractionUpdate{}, errors.New("claude interactive response is missing")
	}
	responseFields, err := response.Object("request_id", "subtype", "response")
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	requestID, err := claudeCurrentRequiredText(responseFields, "request_id", 128)
	if err != nil || requestID != claudeInteractionInitialize {
		return execution.InteractionUpdate{}, errors.New("claude rejected interactive initialization")
	}
	subtype, err := claudeCurrentRequiredText(responseFields, "subtype", 64)
	if err != nil || subtype != "success" || p.initialized {
		return execution.InteractionUpdate{}, errors.New("claude rejected interactive initialization")
	}
	if responseValue, present := responseFields["response"]; present {
		if _, objectErr := responseValue.Object(); objectErr != nil {
			return execution.InteractionUpdate{}, objectErr
		}
	}
	p.initialized = true
	frame, err := json.Marshal(map[string]any{"type": "user", "session_id": p.expected, "parent_tool_use_id": nil, "message": map[string]string{"role": "user", "content": p.brief}})
	p.brief = ""
	return execution.InteractionUpdate{Outbound: [][]byte{frame}}, err
}

func (p *claudeInteraction) controlRequestRecord(fields map[string]providerjson.Value) (execution.InteractionUpdate, error) {
	nativeID, err := claudeCurrentRequiredText(fields, "request_id", 256)
	if err != nil {
		return execution.InteractionUpdate{}, task.ErrInteractionRequest
	}
	request, ok := fields["request"]
	if !ok {
		return execution.InteractionUpdate{}, task.ErrInteractionRequest
	}
	requestFields, err := request.Object("subtype", "tool_name", "agent_id", "tool_use_id", "input")
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	subtype, _, subtypeErr := claudeCurrentOptionalString(requestFields, "subtype", 128)
	toolName, _, toolErr := claudeCurrentOptionalString(requestFields, "tool_name", 256)
	agentID, _, agentErr := claudeCurrentOptionalString(requestFields, "agent_id", 256)
	if subtypeErr != nil || toolErr != nil || agentErr != nil || subtype != "can_use_tool" || toolName != "AskUserQuestion" || agentID != "" {
		frame, frameErr := claudeControlReply(nativeID, map[string]string{"behavior": "deny", "message": "Delegate clarification replies do not grant tool permissions or answer nested agent requests"})
		return execution.InteractionUpdate{Outbound: [][]byte{frame}}, frameErr
	}
	toolUseID, err := claudeCurrentRequiredText(requestFields, "tool_use_id", 256)
	if err != nil {
		return execution.InteractionUpdate{}, task.ErrInteractionRequest
	}
	input, ok := requestFields["input"]
	if !ok {
		return execution.InteractionUpdate{}, task.ErrInteractionRequest
	}
	return p.questionRequestValue(nativeID, toolUseID, input)
}

func (p *claudeInteraction) cancelRequestRecord(fields map[string]providerjson.Value) (execution.InteractionUpdate, error) {
	nativeID, err := claudeCurrentRequiredText(fields, "request_id", 256)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	return p.cancelNativeQuestion(nativeID), nil
}

func (p *claudeInteraction) cancelNativeQuestion(nativeID string) execution.InteractionUpdate {
	id := task.ComputeSHA256([]byte(nativeID))[:32]
	if _, exists := p.pending[id]; !exists {
		return execution.InteractionUpdate{}
	}
	delete(p.pending, id)
	return execution.InteractionUpdate{Events: []task.InteractionEvent{{Kind: "resolved", RequestID: id}}}
}

func (p *claudeInteraction) questionRequestValue(nativeID, toolUseID string, input providerjson.Value) (execution.InteractionUpdate, error) {
	id, questionsValue, err := p.prepareQuestionRequest(nativeID, toolUseID, input)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	pending := claudePendingQuestion{nativeID: nativeID, toolUseID: toolUseID, input: map[string]json.RawMessage{}}
	rawInput, err := input.Raw(maxInteractionInputBytes)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	if err := json.Unmarshal(rawInput, &pending.input); err != nil {
		return execution.InteractionUpdate{}, err
	}
	if err := decodeClaudeCurrentQuestions(questionsValue, &pending); err != nil {
		return execution.InteractionUpdate{}, err
	}
	p.pending[id] = pending
	request := task.InteractionRequest{ID: id, TurnID: p.turnID, Blocking: true, Questions: pending.questions}
	return execution.InteractionUpdate{Events: []task.InteractionEvent{{Kind: "request", Request: &request}}}, nil
}

func (p *claudeInteraction) prepareQuestionRequest(nativeID, toolUseID string, input providerjson.Value) (string, providerjson.Value, error) {
	if !p.state.initSeen || p.finished || len(p.pending) >= 1024 {
		return "", providerjson.Value{}, task.ErrInteractionUnavailable
	}
	id := task.ComputeSHA256([]byte(nativeID))[:32]
	if _, exists := p.pending[id]; exists {
		return "", providerjson.Value{}, task.ErrInteractionConflict
	}
	if toolUseID == "" {
		return "", providerjson.Value{}, task.ErrInteractionRequest
	}
	inputFields, err := input.Object("questions")
	if err != nil {
		return "", providerjson.Value{}, err
	}
	questionsValue, ok := inputFields["questions"]
	if !ok || claudeCurrentValueIsNull(questionsValue) {
		return "", providerjson.Value{}, task.ErrInteractionRequest
	}
	return id, questionsValue, nil
}

func decodeClaudeCurrentQuestions(value providerjson.Value, pending *claudePendingQuestion) error {
	return value.Array(func(questionValue providerjson.Value) error {
		question, err := decodeClaudeCurrentQuestion(questionValue, strconv.Itoa(len(pending.questions)+1))
		if err != nil {
			return err
		}
		pending.questions = append(pending.questions, question)
		return nil
	})
}

func decodeClaudeCurrentQuestion(value providerjson.Value, id string) (task.InteractionQuestion, error) {
	fields, err := value.Object("question", "multiSelect", "options")
	if err != nil {
		return task.InteractionQuestion{}, err
	}
	text, err := claudeCurrentRequiredText(fields, "question", maxInteractionTextBytes)
	if err != nil {
		return task.InteractionQuestion{}, err
	}
	multiple, err := decodeClaudeCurrentMultiSelect(fields)
	if err != nil {
		return task.InteractionQuestion{}, err
	}
	question := task.InteractionQuestion{ID: id, Text: text, Multiple: multiple, FreeText: true}
	if options, present := fields["options"]; present {
		question.Options, err = decodeClaudeCurrentOptions(options)
		if err != nil {
			return task.InteractionQuestion{}, err
		}
	}
	return question, nil
}

func decodeClaudeCurrentMultiSelect(fields map[string]providerjson.Value) (bool, error) {
	raw, present := fields["multiSelect"]
	if !present {
		return false, nil
	}
	multiple, err := claudeCurrentRequiredBool(fields, "multiSelect")
	if err != nil {
		return false, err
	}
	if claudeCurrentValueIsNull(raw) {
		return false, errors.New("question multiSelect is not a boolean")
	}
	return multiple, nil
}

func decodeClaudeCurrentOptions(value providerjson.Value) ([]string, error) {
	var options []string
	if err := value.Array(func(optionValue providerjson.Value) error {
		optionFields, err := optionValue.Object("label")
		if err != nil {
			return err
		}
		label, err := claudeCurrentRequiredText(optionFields, "label", maxInteractionTextBytes)
		if err != nil {
			return err
		}
		options = append(options, label)
		return nil
	}); err != nil {
		return nil, err
	}
	return options, nil
}

func (p *claudeInteraction) observeCurrentEvent(event claudeCurrentEvent) (execution.InteractionUpdate, error) {
	wasInitialized := p.state.initSeen
	if err := p.applyCurrentObservedEvent(event); err != nil {
		return execution.InteractionUpdate{}, err
	}
	if p.state.semanticErr != nil {
		return execution.InteractionUpdate{}, p.state.semanticErr
	}
	return p.finishCurrentObservedEvent(event, wasInitialized)
}

func (p *claudeInteraction) applyCurrentObservedEvent(event claudeCurrentEvent) error {
	if p.finished && (event.typeName == "result" || event.typeName == "assistant" || event.typeName == "user") {
		return task.ErrInteractionClosed
	}
	if event.typeName == "system" {
		return p.applyCurrentSystemEvent(event)
	}
	// A background agent may wake the root with another result. The
	// interaction state machine deliberately allows that one transition,
	// while preserving any earlier provider failure.
	if event.typeName == "result" {
		p.state.resultSeen = false
	}
	applyClaudeCurrentEvent(&p.state, event)
	return p.state.semanticErr
}

func (p *claudeInteraction) applyCurrentSystemEvent(event claudeCurrentEvent) error {
	subtype, present, err := claudeCurrentOptionalString(event.fields, "subtype", 128)
	if err != nil || !present || subtype != "init" || !p.state.initSeen {
		applyClaudeCurrentEvent(&p.state, event)
		return p.state.semanticErr
	}
	state := p.state
	state.initSeen = false
	state.resultSeen = false
	applyClaudeCurrentEvent(&state, event)
	if state.semanticErr != nil || p.finished || !sameClaudeInit(state.init, p.state.init) {
		p.state.markSemantic("system/init changed or followed completion")
		return p.state.semanticErr
	}
	p.state.init = state.init
	p.state.observedSessionID = state.observedSessionID
	p.state.semanticErr = state.semanticErr
	return nil
}

func (p *claudeInteraction) finishCurrentObservedEvent(event claudeCurrentEvent, wasInitialized bool) (execution.InteractionUpdate, error) {
	update := execution.InteractionUpdate{}
	if !wasInitialized && p.state.initSeen {
		ready, err := p.readyEvent()
		if err != nil {
			return update, err
		}
		update.Events = append(update.Events, ready)
	}
	if event.typeName == "user" {
		events, err := p.resolveCurrentToolResults(event.fields)
		if err != nil {
			return update, err
		}
		update.Events = append(update.Events, events...)
	}
	if event.typeName == "result" {
		p.recordResult()
	}
	if event.typeName == "system" {
		if err := p.trackCurrentSystem(event.fields); err != nil {
			return update, err
		}
	}
	if p.isFinished() {
		p.finished = true
		update.Done = true
	}
	return update, nil
}

func (p *claudeInteraction) initializeResponse(line []byte) (execution.InteractionUpdate, error) {
	var envelope struct {
		Response struct {
			RequestID string `json:"request_id"`
			Subtype   string `json:"subtype"`
		} `json:"response"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return execution.InteractionUpdate{}, err
	}
	if envelope.Response.RequestID != claudeInteractionInitialize || envelope.Response.Subtype != "success" || p.initialized {
		return execution.InteractionUpdate{}, errors.New("claude rejected interactive initialization")
	}
	p.initialized = true
	frame, err := json.Marshal(map[string]any{"type": "user", "session_id": p.expected, "parent_tool_use_id": nil, "message": map[string]string{"role": "user", "content": p.brief}})
	p.brief = ""
	return execution.InteractionUpdate{Outbound: [][]byte{frame}}, err
}

func (p *claudeInteraction) observeEvent(event parsedEvent) (execution.InteractionUpdate, error) {
	wasInitialized := p.state.initSeen
	if p.finished && (event.typeName == "result" || event.typeName == "assistant" || event.typeName == "user") {
		return execution.InteractionUpdate{}, task.ErrInteractionClosed
	}
	// A native background agent can keep the same input turn alive beyond an
	// intermediate result. Retain the latest result without inventing user turns.
	p.state.resultSeen = false
	p.applyEvent(event)
	if p.state.semanticErr != nil {
		return execution.InteractionUpdate{}, p.state.semanticErr
	}
	update := execution.InteractionUpdate{}
	if !wasInitialized && p.state.initSeen {
		ready, err := p.readyEvent()
		if err != nil {
			return update, err
		}
		update.Events = append(update.Events, ready)
	}
	if event.typeName == "user" {
		events, err := p.resolveToolResults(event.fields)
		if err != nil {
			return update, err
		}
		update.Events = append(update.Events, events...)
	}
	if event.typeName == "result" {
		p.recordResult()
	}
	if event.typeName == "system" {
		if err := p.trackSystem(event.fields); err != nil {
			return update, err
		}
	}
	if p.isFinished() {
		p.finished = true
		update.Done = true
	}
	return update, nil
}

func (p *claudeInteraction) applyEvent(event parsedEvent) {
	subtype, _, _ := readOptionalString(event.fields, "subtype")
	if event.typeName != "system" || subtype != "init" || !p.state.initSeen {
		applyEvent(&p.state, event)
		return
	}
	// Claude repeats system/init when background work wakes the same session.
	// Validate the new envelope without publishing another ready event or
	// resetting the original session, pending questions, or failure evidence.
	state := p.state
	state.initSeen = false
	applyEvent(&state, event)
	if p.finished || state.init.cwd != p.state.init.cwd {
		state.markSemantic("system/init changed or followed completion")
	}
	p.state.semanticErr = state.semanticErr
}

func (p *claudeInteraction) trackSystem(fields map[string]json.RawMessage) error {
	subtype, _, valid := readOptionalString(fields, "subtype")
	if !valid {
		return task.ErrEvidenceFault
	}
	switch subtype {
	case "session_state_changed":
		state, err := requiredString(fields, "state")
		if err != nil {
			return err
		}
		if state != "idle" && state != "running" && state != "requires_action" {
			return task.ErrEvidenceFault
		}
		p.sessionState = state
	case "task_started":
		return p.trackAgentStart(fields)
	case "task_notification":
		id, err := requiredString(fields, "task_id")
		if err != nil {
			return err
		}
		p.settleAgent(id)
	case "task_updated":
		return p.trackTaskUpdate(fields)
	}
	return nil
}

func (p *claudeInteraction) trackTaskUpdate(fields map[string]json.RawMessage) error {
	var patch struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(fields["patch"], &patch); err != nil {
		return err
	}
	if patch.Status == "completed" || patch.Status == "failed" || patch.Status == "stopped" {
		id, err := requiredString(fields, "task_id")
		if err != nil {
			return err
		}
		p.settleAgent(id)
	}
	return nil
}

func claudeControlReply(id string, response any) ([]byte, error) {
	return json.Marshal(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": response}})
}

func (p *claudeInteraction) controlRequest(line []byte) (execution.InteractionUpdate, error) {
	var envelope struct {
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype   string          `json:"subtype"`
			ToolName  string          `json:"tool_name"`
			AgentID   string          `json:"agent_id"`
			ToolUseID string          `json:"tool_use_id"`
			Input     json.RawMessage `json:"input"`
		} `json:"request"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return execution.InteractionUpdate{}, err
	}
	if envelope.RequestID == "" {
		return execution.InteractionUpdate{}, task.ErrInteractionRequest
	}
	if envelope.Request.Subtype != "can_use_tool" || envelope.Request.ToolName != "AskUserQuestion" || envelope.Request.AgentID != "" {
		frame, err := claudeControlReply(envelope.RequestID, map[string]string{"behavior": "deny", "message": "Delegate clarification replies do not grant tool permissions or answer nested agent requests"})
		return execution.InteractionUpdate{Outbound: [][]byte{frame}}, err
	}
	return p.questionRequest(envelope.RequestID, envelope.Request.ToolUseID, envelope.Request.Input)
}

func (p *claudeInteraction) questionRequest(nativeID, toolUseID string, input json.RawMessage) (execution.InteractionUpdate, error) {
	if !p.state.initSeen || p.finished || len(p.pending) >= 1024 {
		return execution.InteractionUpdate{}, task.ErrInteractionUnavailable
	}
	id := task.ComputeSHA256([]byte(nativeID))[:32]
	if _, exists := p.pending[id]; exists {
		return execution.InteractionUpdate{}, task.ErrInteractionConflict
	}
	var decoded claudeQuestionInput
	if err := json.Unmarshal(input, &decoded); err != nil {
		return execution.InteractionUpdate{}, err
	}
	pending := claudePendingQuestion{nativeID: nativeID, toolUseID: toolUseID}
	if toolUseID == "" {
		return execution.InteractionUpdate{}, task.ErrInteractionRequest
	}
	if err := json.Unmarshal(input, &pending.input); err != nil {
		return execution.InteractionUpdate{}, err
	}
	for index, q := range decoded.Questions {
		question := task.InteractionQuestion{ID: strconv.Itoa(index + 1), Text: q.Question, Multiple: q.MultiSelect, FreeText: true}
		for _, option := range q.Options {
			question.Options = append(question.Options, option.Label)
		}
		pending.questions = append(pending.questions, question)
	}
	p.pending[id] = pending
	request := task.InteractionRequest{ID: id, TurnID: p.turnID, Blocking: true, Questions: pending.questions}
	return execution.InteractionUpdate{Events: []task.InteractionEvent{{Kind: "request", Request: &request}}}, nil
}

func (p *claudeInteraction) cancelRequest(fields map[string]json.RawMessage) (execution.InteractionUpdate, error) {
	nativeID, err := requiredString(fields, "request_id")
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	id := task.ComputeSHA256([]byte(nativeID))[:32]
	if _, exists := p.pending[id]; !exists {
		return execution.InteractionUpdate{}, nil
	}
	delete(p.pending, id)
	return execution.InteractionUpdate{Events: []task.InteractionEvent{{Kind: "resolved", RequestID: id}}}, nil
}

func (p *claudeInteraction) Encode(message task.InteractionMessage) ([]byte, error) {
	if message.Kind != "reply" {
		return nil, task.ErrInteractionUnavailable
	}
	if p.finished || message.TurnID != p.turnID {
		return nil, task.ErrInteractionClosed
	}
	pending, exists := p.pending[message.RequestID]
	if !exists {
		return nil, task.ErrInteractionRequest
	}
	answers := map[string]string{}
	for _, question := range pending.questions {
		answers[question.Text] = strings.Join(message.Answers[question.ID], ", ")
	}
	encoded, err := json.Marshal(answers)
	if err != nil {
		return nil, err
	}
	pending.input["answers"] = encoded
	pending.messageID = message.ID
	p.pending[message.RequestID] = pending
	return claudeControlReply(pending.nativeID, map[string]any{"behavior": "allow", "updatedInput": pending.input})
}

func (p *claudeInteraction) Complete() error {
	if !p.finished || !p.resultSeen || p.failed || p.state.semanticErr != nil {
		return task.ErrEvidenceFault
	}
	return nil
}

func (p *claudeInteraction) resolveToolResults(fields map[string]json.RawMessage) ([]task.InteractionEvent, error) {
	var message struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(fields["message"], &message); err != nil {
		return nil, err
	}
	if len(message.Content) == 0 || message.Content[0] != '[' {
		return nil, nil
	}
	var blocks []struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
		IsError   bool   `json:"is_error"`
	}
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return nil, err
	}
	var events []task.InteractionEvent
	for _, block := range blocks {
		if block.Type != "tool_result" {
			continue
		}
		for id, pending := range p.pending {
			if pending.toolUseID != block.ToolUseID {
				continue
			}
			if pending.messageID != "" {
				status := "acknowledged"
				if block.IsError {
					status = "rejected"
				}
				events = append(events, task.InteractionEvent{Kind: "delivery", Delivery: &task.InteractionDelivery{MessageID: pending.messageID, State: status}})
			}
			events = append(events, task.InteractionEvent{Kind: "resolved", RequestID: id})
			delete(p.pending, id)
		}
	}
	return events, nil
}

func (p *claudeInteraction) resolveCurrentToolResults(fields map[string]providerjson.Value) ([]task.InteractionEvent, error) {
	message, ok := fields["message"]
	if !ok || claudeCurrentValueIsNull(message) {
		return nil, nil
	}
	messageFields, err := message.Object("content")
	if err != nil {
		return nil, err
	}
	content, ok := messageFields["content"]
	if !ok || claudeCurrentValueIsNull(content) {
		return nil, nil
	}
	var events []task.InteractionEvent
	if err := content.Array(func(block providerjson.Value) error {
		return p.resolveCurrentToolResultBlock(block, &events)
	}); err != nil {
		return nil, err
	}
	return events, nil
}

func (p *claudeInteraction) resolveCurrentToolResultBlock(block providerjson.Value, events *[]task.InteractionEvent) error {
	blockFields, err := block.Object("type", "tool_use_id", "is_error")
	if err != nil {
		return err
	}
	kind, validKind := claudeCurrentToolResultKind(blockFields)
	if !validKind || kind != "tool_result" {
		return nil
	}
	toolUseID, err := claudeCurrentRequiredText(blockFields, "tool_use_id", 1024)
	if err != nil {
		return err
	}
	isError, err := decodeClaudeCurrentToolResultError(blockFields)
	if err != nil {
		return err
	}
	p.appendCurrentToolResultEvents(toolUseID, isError, events)
	return nil
}

func claudeCurrentToolResultKind(fields map[string]providerjson.Value) (string, bool) {
	kind, err := claudeCurrentRequiredText(fields, "type", 64)
	return kind, err == nil
}

func decodeClaudeCurrentToolResultError(fields map[string]providerjson.Value) (bool, error) {
	raw, present := fields["is_error"]
	if !present {
		return false, nil
	}
	isError, err := claudeCurrentRequiredBool(fields, "is_error")
	if err != nil {
		return false, err
	}
	if claudeCurrentValueIsNull(raw) {
		return false, errors.New("tool_result is_error is not a boolean")
	}
	return isError, nil
}

func (p *claudeInteraction) appendCurrentToolResultEvents(toolUseID string, isError bool, events *[]task.InteractionEvent) {
	for id, pending := range p.pending {
		if pending.toolUseID != toolUseID {
			continue
		}
		if pending.messageID != "" {
			status := "acknowledged"
			if isError {
				status = "rejected"
			}
			*events = append(*events, task.InteractionEvent{Kind: "delivery", Delivery: &task.InteractionDelivery{MessageID: pending.messageID, State: status}})
		}
		*events = append(*events, task.InteractionEvent{Kind: "resolved", RequestID: id})
		delete(p.pending, id)
	}
}

func (p *claudeInteraction) trackAgentStart(fields map[string]json.RawMessage) error {
	kind, _, ok := readOptionalString(fields, "task_type")
	if !ok {
		return task.ErrEvidenceFault
	}
	if kind == "local_agent" || kind == "local_workflow" {
		id, err := requiredString(fields, "task_id")
		if err != nil {
			return err
		}
		if len(p.agents) >= 1024 {
			return task.ErrEvidenceFault
		}
		p.agents[id] = true
	}
	return nil
}

func (p *claudeInteraction) trackCurrentSystem(fields map[string]providerjson.Value) error {
	subtype, _, err := claudeCurrentOptionalString(fields, "subtype", 128)
	if err != nil {
		return task.ErrEvidenceFault
	}
	switch subtype {
	case "session_state_changed":
		return p.trackCurrentSessionState(fields)
	case "task_started":
		return p.trackCurrentTaskStarted(fields)
	case "task_notification":
		return p.trackCurrentTaskNotification(fields)
	case "task_updated":
		return p.trackCurrentTaskUpdated(fields)
	}
	return nil
}

func (p *claudeInteraction) trackCurrentSessionState(fields map[string]providerjson.Value) error {
	state, err := claudeCurrentRequiredText(fields, "state", 64)
	if err != nil || (state != "idle" && state != "running" && state != "requires_action") {
		return task.ErrEvidenceFault
	}
	p.sessionState = state
	return nil
}

func (p *claudeInteraction) trackCurrentTaskStarted(fields map[string]providerjson.Value) error {
	kind, present, err := claudeCurrentOptionalString(fields, "task_type", 128)
	if err != nil || !present {
		return task.ErrEvidenceFault
	}
	if kind != "local_agent" && kind != "local_workflow" {
		return nil
	}
	if len(p.agents) >= 1024 {
		return task.ErrEvidenceFault
	}
	id, err := claudeCurrentRequiredText(fields, "task_id", 1024)
	if err != nil {
		return task.ErrEvidenceFault
	}
	p.agents[id] = true
	return nil
}

func (p *claudeInteraction) trackCurrentTaskNotification(fields map[string]providerjson.Value) error {
	id, err := claudeCurrentRequiredText(fields, "task_id", 1024)
	if err != nil {
		return err
	}
	p.settleAgent(id)
	return nil
}

func (p *claudeInteraction) trackCurrentTaskUpdated(fields map[string]providerjson.Value) error {
	patch, ok := fields["patch"]
	if !ok {
		return task.ErrEvidenceFault
	}
	patchFields, err := patch.Object("status")
	if err != nil {
		return err
	}
	status, _, err := claudeCurrentOptionalString(patchFields, "status", 64)
	if err != nil {
		return err
	}
	if status != "completed" && status != "failed" && status != "stopped" {
		return nil
	}
	id, err := claudeCurrentRequiredText(fields, "task_id", 1024)
	if err != nil {
		return err
	}
	p.settleAgent(id)
	return nil
}

func sameClaudeInit(left, right initState) bool {
	if left.sessionID != right.sessionID || left.version != right.version || left.cwd != right.cwd || left.permission != right.permission || left.apiKeySource != right.apiKeySource || left.model != right.model || left.mcpServerCount != right.mcpServerCount || len(left.tools) != len(right.tools) {
		return false
	}
	for index := range left.tools {
		if left.tools[index] != right.tools[index] {
			return false
		}
	}
	return true
}

func (p *claudeInteraction) isFinished() bool {
	return p.resultSeen && len(p.agents) == 0 && len(p.pending) == 0 && (p.sessionState == "" || p.sessionState == "idle")
}

func (p *claudeInteraction) readyEvent() (task.InteractionEvent, error) {
	if p.state.init.sessionID != p.expected {
		return task.InteractionEvent{}, task.ErrIdentityMismatch
	}
	if p.record != nil {
		if err := p.record(task.SessionIdentity{Provider: Provider, ConversationID: p.expected}); err != nil {
			return task.InteractionEvent{}, err
		}
	}
	caps := task.InteractionCapabilities{Clarification: true, SupportedModes: []string{p.state.mode}}
	return task.InteractionEvent{Kind: "ready", TurnID: p.turnID, Capabilities: &caps}, nil
}

func (p *claudeInteraction) settleAgent(id string) {
	if p.agents[id] {
		// Completion wakes the parent for a follow-up within this invocation.
		// An earlier result and idle event cannot cover that new work.
		p.resultSeen = false
		delete(p.agents, id)
	}
}

func (p *claudeInteraction) recordResult() {
	p.resultSeen = true
	refusal := resultRefusal(p.state.result)
	p.failed = p.failed || (refusal != "" && refusal != refusalEmpty)
}
