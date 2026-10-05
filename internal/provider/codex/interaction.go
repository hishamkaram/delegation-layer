package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type appServerEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type appServerProtocol struct {
	request   task.TaskRecord
	expected  task.SessionExpectation
	record    func(task.SessionIdentity) error
	brief     string
	phase     int
	threadID  string
	turnID    string
	completed bool
	failed    bool
	replay    bool
	final     string
	requests  map[string]json.RawMessage
	steering  map[string]bool
}

func newAppServerProtocol(request task.TaskRecord, expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.InteractionProtocol, error) {
	if expected.Required && !validThreadID(expected.ID) {
		return nil, task.ErrIdentityMismatch
	}
	if !expected.Required && expected.ID != "" {
		return nil, task.ErrIdentityMismatch
	}
	return &appServerProtocol{request: request, expected: expected, record: record, requests: map[string]json.RawMessage{}, steering: map[string]bool{}}, nil
}

func appServerFrame(id, method string, params any) ([]byte, error) {
	return json.Marshal(struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{id, method, params})
}

func (p *appServerProtocol) Start(brief []byte) ([][]byte, error) {
	if p.phase != 0 || len(brief) == 0 || !utf8.Valid(brief) {
		return nil, task.ErrInteractionRequest
	}
	p.brief = string(brief)
	p.phase = 1
	frame, err := appServerFrame("delegate-init", "initialize", map[string]any{"clientInfo": map[string]string{"name": "delegation-layer", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}})
	return [][]byte{frame}, err
}

func (p *appServerProtocol) Receive(line []byte) (execution.InteractionUpdate, error) {
	if !utf8.Valid(line) {
		return execution.InteractionUpdate{}, task.ErrEvidenceFault
	}
	if err := task.ValidateJSONStructure(line); err != nil {
		return execution.InteractionUpdate{}, err
	}
	var envelope appServerEnvelope
	if err := json.Unmarshal(line, &envelope); err != nil {
		return execution.InteractionUpdate{}, err
	}
	if envelope.Method != "" {
		return p.notification(envelope)
	}
	return p.response(envelope)
}

func (p *appServerProtocol) response(envelope appServerEnvelope) (execution.InteractionUpdate, error) {
	var id string
	if err := json.Unmarshal(envelope.ID, &id); err != nil {
		return execution.InteractionUpdate{}, err
	}
	if strings.HasPrefix(id, "delegate-steer-") {
		return p.steerResponse(id, envelope)
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return execution.InteractionUpdate{}, errors.New("codex rejected interactive protocol setup")
	}
	switch id {
	case "delegate-init":
		return p.startThread()
	case "delegate-thread":
		return p.startTurn(envelope.Result)
	case "delegate-turn":
		return p.turnResponse(envelope.Result)
	default:
		return execution.InteractionUpdate{}, task.ErrInteractionRequest
	}
}

func (p *appServerProtocol) startThread() (execution.InteractionUpdate, error) {
	if p.phase != 1 {
		return execution.InteractionUpdate{}, task.ErrInteractionConflict
	}
	p.phase = 2
	params := map[string]any{"cwd": p.request.CanonicalCwd, "approvalPolicy": "never", "sandbox": p.request.Mode}
	if p.request.RequestedConfig.Model != "" {
		params["model"] = p.request.RequestedConfig.Model
	}
	method := "thread/start"
	if p.expected.Required {
		method = "thread/resume"
		params["threadId"] = p.expected.ID
	}
	frame, err := appServerFrame("delegate-thread", method, params)
	return execution.InteractionUpdate{Outbound: [][]byte{[]byte(`{"method":"initialized"}`), frame}}, err
}

func (p *appServerProtocol) startTurn(result json.RawMessage) (execution.InteractionUpdate, error) {
	if p.phase != 2 {
		return execution.InteractionUpdate{}, task.ErrInteractionConflict
	}
	var response struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return execution.InteractionUpdate{}, err
	}
	if !identityMatchesExpectation(p.expected, response.Thread.ID) {
		return execution.InteractionUpdate{}, task.ErrIdentityMismatch
	}
	p.threadID = response.Thread.ID
	p.phase = 3
	if p.record != nil {
		if err := p.record(task.SessionIdentity{Provider: Provider, ConversationID: p.threadID}); err != nil {
			return execution.InteractionUpdate{}, err
		}
	}
	params := map[string]any{"threadId": p.threadID, "input": appServerText(p.brief)}
	if effort := p.request.RequestedConfig.Effort; effort != "" && effort != "default" {
		params["effort"] = effort
	}
	frame, err := appServerFrame("delegate-turn", "turn/start", params)
	p.brief = ""
	return execution.InteractionUpdate{Outbound: [][]byte{frame}}, err
}

func appServerText(text string) []any {
	return []any{map[string]any{"type": "text", "text": text, "textElements": []any{}}}
}

func (p *appServerProtocol) turnResponse(result json.RawMessage) (execution.InteractionUpdate, error) {
	var response struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return execution.InteractionUpdate{}, err
	}
	return p.activateTurn(response.Turn.ID)
}

func (p *appServerProtocol) activateTurn(id string) (execution.InteractionUpdate, error) {
	if p.phase != 3 || id == "" || (p.turnID != "" && p.turnID != id) {
		return execution.InteractionUpdate{}, task.ErrInteractionConflict
	}
	if p.turnID == id {
		return execution.InteractionUpdate{}, nil
	}
	p.turnID = id
	caps := task.InteractionCapabilities{SupportedModes: []string{p.request.Mode}, Clarification: true, Steering: true}
	return execution.InteractionUpdate{Events: []task.InteractionEvent{{Kind: "ready", TurnID: id, Capabilities: &caps}}}, nil
}

func (p *appServerProtocol) notification(envelope appServerEnvelope) (execution.InteractionUpdate, error) {
	switch envelope.Method {
	case "turn/started":
		var params struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if err := json.Unmarshal(envelope.Params, &params); err != nil {
			return execution.InteractionUpdate{}, err
		}
		if params.ThreadID != p.threadID {
			return execution.InteractionUpdate{}, nil
		}
		if p.completed {
			return execution.InteractionUpdate{}, task.ErrInteractionClosed
		}
		return p.activateTurn(params.Turn.ID)
	case "turn/completed":
		return p.completeTurn(envelope.Params)
	case "item/completed":
		return execution.InteractionUpdate{}, p.completeItem(envelope.Params)
	case "item/tool/requestUserInput":
		return p.requestInput(envelope)
	case "serverRequest/resolved":
		return p.resolveRequest(envelope.Params)
	case "error":
		return execution.InteractionUpdate{}, p.observeError(envelope.Params)
	default:
		if len(envelope.ID) > 0 {
			return denyAppServerRequest(envelope.ID)
		}
		return execution.InteractionUpdate{}, nil
	}
}

func denyAppServerRequest(id json.RawMessage) (execution.InteractionUpdate, error) {
	frame, err := json.Marshal(map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "Delegate clarification replies do not grant tool permissions"}})
	return execution.InteractionUpdate{Outbound: [][]byte{frame}}, err
}

func (p *appServerProtocol) completeTurn(data json.RawMessage) (execution.InteractionUpdate, error) {
	var params struct {
		ThreadID string `json:"threadId"`
		Turn     struct {
			ID     string          `json:"id"`
			Status string          `json:"status"`
			Error  json.RawMessage `json:"error"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(data, &params); err != nil {
		return execution.InteractionUpdate{}, err
	}
	if params.ThreadID != p.threadID {
		return execution.InteractionUpdate{}, nil
	}
	if p.turnID == "" || params.Turn.ID != p.turnID || p.completed {
		return execution.InteractionUpdate{}, task.ErrInteractionConflict
	}
	p.completed = true
	p.failed = p.failed || params.Turn.Status != "completed" || (len(params.Turn.Error) > 0 && string(params.Turn.Error) != "null")
	return execution.InteractionUpdate{Done: true}, nil
}

func (p *appServerProtocol) completeItem(data json.RawMessage) error {
	var params struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Item     struct {
			Type  string `json:"type"`
			Text  string `json:"text"`
			Phase string `json:"phase"`
		} `json:"item"`
	}
	if err := json.Unmarshal(data, &params); err != nil {
		return err
	}
	if params.ThreadID != p.threadID {
		return nil
	}
	if params.TurnID != p.turnID || p.completed {
		return task.ErrInteractionConflict
	}
	if params.Item.Type == "agentMessage" && params.Item.Phase != "commentary" {
		p.final = params.Item.Text
	}
	return nil
}

type appServerQuestion struct {
	ID       string `json:"id"`
	Question string `json:"question"`
	IsOther  bool   `json:"isOther"`
	Options  []struct {
		Label string `json:"label"`
	} `json:"options"`
}

func (p *appServerProtocol) requestInput(envelope appServerEnvelope) (execution.InteractionUpdate, error) {
	var params struct {
		ThreadID  string              `json:"threadId"`
		TurnID    string              `json:"turnId"`
		Blocking  bool                `json:"isBlocking"`
		Questions []appServerQuestion `json:"questions"`
	}
	if err := json.Unmarshal(envelope.Params, &params); err != nil {
		return execution.InteractionUpdate{}, err
	}
	if params.ThreadID != p.threadID {
		return denyAppServerRequest(envelope.ID)
	}
	if p.completed || params.TurnID != p.turnID || len(envelope.ID) == 0 || len(p.requests) >= maxTrackedItems {
		return execution.InteractionUpdate{}, task.ErrInteractionRequest
	}
	id := task.ComputeSHA256(envelope.ID)[:32]
	if _, exists := p.requests[id]; exists {
		return execution.InteractionUpdate{}, task.ErrInteractionConflict
	}
	p.requests[id] = envelope.ID
	request := task.InteractionRequest{ID: id, TurnID: p.turnID, Blocking: params.Blocking}
	for _, question := range params.Questions {
		q := task.InteractionQuestion{ID: question.ID, Text: question.Question, FreeText: question.IsOther || len(question.Options) == 0}
		for _, option := range question.Options {
			q.Options = append(q.Options, option.Label)
		}
		request.Questions = append(request.Questions, q)
	}
	return execution.InteractionUpdate{Events: []task.InteractionEvent{{Kind: "request", Request: &request}}}, nil
}

func (p *appServerProtocol) resolveRequest(data json.RawMessage) (execution.InteractionUpdate, error) {
	var params struct {
		ThreadID  string          `json:"threadId"`
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := json.Unmarshal(data, &params); err != nil {
		return execution.InteractionUpdate{}, err
	}
	if params.ThreadID != p.threadID {
		return execution.InteractionUpdate{}, nil
	}
	id := task.ComputeSHA256(params.RequestID)[:32]
	if _, exists := p.requests[id]; !exists {
		return execution.InteractionUpdate{}, nil
	}
	delete(p.requests, id)
	// Resolution can also mean native cancellation; it is not proof that an
	// answer was consumed. The separate sent receipt intentionally stays sent.
	return execution.InteractionUpdate{Events: []task.InteractionEvent{{Kind: "resolved", RequestID: id}}}, nil
}

func (p *appServerProtocol) Encode(message task.InteractionMessage) ([]byte, error) {
	if p.completed || message.TurnID != p.turnID {
		return nil, task.ErrInteractionClosed
	}
	switch message.Kind {
	case "steer":
		if len(p.steering) >= maxTrackedItems {
			return nil, task.ErrInteractionUnavailable
		}
		p.steering[message.ID] = true
		return appServerFrame("delegate-steer-"+message.ID, "turn/steer", map[string]any{"threadId": p.threadID, "expectedTurnId": p.turnID, "input": appServerText(message.Text)})
	case "reply":
		nativeID, exists := p.requests[message.RequestID]
		if !exists {
			return nil, task.ErrInteractionRequest
		}
		answers := map[string]any{}
		for id, values := range message.Answers {
			answers[id] = map[string]any{"answers": values}
		}
		return json.Marshal(map[string]any{"id": nativeID, "result": map[string]any{"answers": answers}})
	default:
		return nil, task.ErrInteractionRequest
	}
}

func (p *appServerProtocol) steerResponse(id string, envelope appServerEnvelope) (execution.InteractionUpdate, error) {
	messageID := strings.TrimPrefix(id, "delegate-steer-")
	if p.replay {
		return execution.InteractionUpdate{}, task.ValidateTaskID(messageID)
	}
	if !p.steering[messageID] {
		return execution.InteractionUpdate{}, task.ErrInteractionRequest
	}
	delete(p.steering, messageID)
	delivery := task.InteractionDelivery{MessageID: messageID, State: "acknowledged"}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		delivery.State = "rejected"
		delivery.Reason = "Codex rejected steering"
	} else {
		var result struct {
			TurnID string `json:"turnId"`
		}
		if err := json.Unmarshal(envelope.Result, &result); err != nil {
			return execution.InteractionUpdate{}, err
		}
		if result.TurnID != p.turnID {
			return execution.InteractionUpdate{}, task.ErrIdentityMismatch
		}
	}
	return execution.InteractionUpdate{Events: []task.InteractionEvent{{Kind: "delivery", Delivery: &delivery}}}, nil
}

func (p *appServerProtocol) Complete() error {
	if !p.completed || p.failed {
		return fmt.Errorf("%w: Codex interactive turn did not complete successfully", task.ErrEvidenceFault)
	}
	return nil
}

func (p *appServerProtocol) observeError(data json.RawMessage) error {
	var params struct {
		ThreadID  string `json:"threadId"`
		TurnID    string `json:"turnId"`
		WillRetry *bool  `json:"willRetry"`
		Error     *struct {
			Message *string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &params); err != nil {
		return err
	}
	if params.ThreadID == "" || params.TurnID == "" || params.WillRetry == nil || params.Error == nil || params.Error.Message == nil {
		return task.ErrEvidenceFault
	}
	if params.ThreadID != p.threadID {
		return nil
	}
	if params.TurnID != p.turnID {
		return task.ErrIdentityMismatch
	}
	p.failed = p.failed || !*params.WillRetry
	return nil
}
