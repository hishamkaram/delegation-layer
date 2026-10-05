package claude

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const claudeInteractionInitialize = "delegate-initialize"

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
	turnID       string
	expected     string
	record       func(task.SessionIdentity) error
	state        eventState
	started      bool
	initialized  bool
	finished     bool
	brief        string
	sessionState string
	resultSeen   bool
	failed       bool
	pending      map[string]claudePendingQuestion
	agents       map[string]bool
}

func newClaudeInteraction(request task.TaskRecord, expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.InteractionProtocol, error) {
	id, err := expectedSessionID(request.RootID, request.TaskID, expected)
	if err != nil {
		return nil, err
	}
	return &claudeInteraction{turnID: request.TaskID, expected: id, record: record, state: eventState{mode: request.Mode}, pending: map[string]claudePendingQuestion{}, agents: map[string]bool{}}, nil
}

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
