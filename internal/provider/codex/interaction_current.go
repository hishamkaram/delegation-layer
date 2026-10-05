package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func evaluateCurrentInteraction(v interactionInterpreter, input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	return evaluateCurrentInteractionAgainst(v, input, raw, out, v.Reference())
}

func evaluateCurrentInteractionAgainst(v interactionInterpreter, input predicate.Input, raw predicate.Evidence, out io.Writer, reference task.PredicateRef) (task.Interpretation, error) {
	if !input.Seal.Predicate.Equal(reference) {
		return task.Interpretation{}, task.ErrIdentityMismatch
	}
	if err := task.ValidateProviderExitRecord(&input.Seal); err != nil {
		return task.Interpretation{}, err
	}
	if raw == nil || out == nil {
		return task.Interpretation{}, task.ErrEvidenceFault
	}
	p := &appServerProtocol{expected: input.ExpectedSession, phase: 1, requests: map[string]json.RawMessage{}, steering: map[string]bool{}, agentItems: make(map[string]currentItemState), replay: true}
	readErr, semanticErr := readCurrentInteraction(raw, p)
	if readErr != nil {
		return task.Interpretation{}, readErr
	}
	if refusal := sealRefusal(input.Seal); refusal != "" {
		result := rejected(refusal)
		result.Diagnostics = []task.OutputDiagnostic{currentDiagnostic(nil, refusal)}
		return result, nil
	}
	completeErr := p.Complete()
	if semanticErr == nil {
		semanticErr = completeErr
	}
	if semanticErr != nil {
		result := rejected(refusalIncomplete)
		result.Diagnostics = []task.OutputDiagnostic{currentDiagnostic(semanticErr, refusalIncomplete)}
		return currentInteractionRejection(result)
	}
	if !identityMatches(input, p.threadID) {
		result := rejected(refusalIdentity)
		result.Diagnostics = []task.OutputDiagnostic{currentDiagnostic(nil, refusalIdentity)}
		return result, nil
	}
	if !p.finalSeen || !p.finalNonblank {
		result := rejected(refusalEmpty)
		result.Diagnostics = []task.OutputDiagnostic{currentDiagnostic(nil, refusalEmpty)}
		return result, nil
	}
	if _, err := streamCurrentAppServerFinal(raw, p.finalStart, p.finalEnd, io.Discard); err != nil {
		return task.Interpretation{}, err
	}
	if _, err := streamCurrentAppServerFinal(raw, p.finalStart, p.finalEnd, out); err != nil {
		return task.Interpretation{}, err
	}
	return task.Interpretation{Verdict: task.VerdictCommitted, Session: &task.SessionIdentity{Provider: Provider, ConversationID: p.threadID}}, nil
}

func currentInteractionRejection(result task.Interpretation) (task.Interpretation, error) {
	return result, nil
}

func (v interactionInterpreter) CanRecover(source task.PredicateRef) bool {
	return !v.historical && source.Equal(historicalInteractionReference(v.mode))
}

// Target is the explicit current predicate selected by recovery. Historical
// interactive registrations remain replay-only because CanRecover is false.
func (v interactionInterpreter) Target() task.PredicateRef { return v.Reference() }

func (v interactionInterpreter) Recover(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if !v.CanRecover(input.Seal.Predicate) {
		return task.Interpretation{}, task.ErrIncompatiblePredicate
	}
	return evaluateCurrentInteractionAgainst(v, input, raw, out, input.Seal.Predicate)
}

// ReceiveRecord is the corrected app-server path. It parses selected JSON-RPC
// control fields from a callback-scoped record and retains only lifecycle
// state, request IDs, and the offset of the final root agent message.
func (p *appServerProtocol) ReceiveRecord(value providerjson.Value) (execution.InteractionUpdate, error) {
	fields, err := value.Object("id", "method", "params", "result", "error")
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	methodValue, hasMethod := fields["method"]
	if hasMethod {
		method, methodErr := methodValue.String(maxItemTypeBytes)
		if methodErr != nil || method == "" {
			return execution.InteractionUpdate{}, errors.New("codex app-server method is not a nonempty string")
		}
		var nativeID []byte
		if idValue, ok := fields["id"]; ok {
			nativeID, err = idValue.Raw(maxInteractionNativeID)
			if err != nil {
				return execution.InteractionUpdate{}, err
			}
		}
		return p.currentNotification(method, nativeID, fields, value)
	}
	return p.currentResponse(fields)
}

func (p *appServerProtocol) currentResponse(fields map[string]providerjson.Value) (execution.InteractionUpdate, error) {
	idValue, ok := fields["id"]
	if !ok {
		return execution.InteractionUpdate{}, task.ErrInteractionRequest
	}
	rawID, err := idValue.Raw(maxInteractionNativeID)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	id, err := currentJSONIDString(rawID)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	if strings.HasPrefix(id, "delegate-steer-") {
		return p.currentSteerResponse(id, fields)
	}
	if errorValue, exists := fields["error"]; exists {
		null, nullErr := currentValueIsNull(errorValue)
		if nullErr != nil {
			return execution.InteractionUpdate{}, nullErr
		}
		if !null {
			return execution.InteractionUpdate{}, errors.New("codex rejected interactive protocol setup")
		}
	}
	result, resultOK := fields["result"]
	switch id {
	case "delegate-init":
		return p.startThread()
	case "delegate-thread":
		if !resultOK {
			return execution.InteractionUpdate{}, task.ErrInteractionRequest
		}
		return p.startTurnRecord(result)
	case "delegate-turn":
		if !resultOK {
			return execution.InteractionUpdate{}, task.ErrInteractionRequest
		}
		return p.turnResponseRecord(result)
	default:
		return execution.InteractionUpdate{}, task.ErrInteractionRequest
	}
}

func (p *appServerProtocol) startTurnRecord(result providerjson.Value) (execution.InteractionUpdate, error) {
	if p.phase != 2 {
		return execution.InteractionUpdate{}, task.ErrInteractionConflict
	}
	fields, err := result.Object("thread")
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	threadValue, ok := fields["thread"]
	if !ok {
		return execution.InteractionUpdate{}, task.ErrIdentityMismatch
	}
	threadFields, err := threadValue.Object("id")
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	threadID, err := currentRequiredString(threadFields, "id", maxItemIDBytes)
	if err != nil {
		return execution.InteractionUpdate{}, task.ErrIdentityMismatch
	}
	if !identityMatchesExpectation(p.expected, threadID) {
		return execution.InteractionUpdate{}, task.ErrIdentityMismatch
	}
	p.threadID = threadID
	p.phase = 3
	if p.record != nil {
		if err = p.record(task.SessionIdentity{Provider: Provider, ConversationID: p.threadID}); err != nil {
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

func (p *appServerProtocol) turnResponseRecord(result providerjson.Value) (execution.InteractionUpdate, error) {
	fields, err := result.Object("turn")
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	turnValue, ok := fields["turn"]
	if !ok {
		return execution.InteractionUpdate{}, task.ErrInteractionConflict
	}
	turnFields, err := turnValue.Object("id")
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	turnID, err := currentRequiredString(turnFields, "id", maxItemIDBytes)
	if err != nil {
		return execution.InteractionUpdate{}, task.ErrInteractionConflict
	}
	return p.activateTurn(turnID)
}

func (p *appServerProtocol) currentNotification(method string, nativeID []byte, fields map[string]providerjson.Value, record providerjson.Value) (execution.InteractionUpdate, error) {
	switch method {
	case "turn/started":
		return p.currentTurnStarted(fields)
	case "turn/completed":
		return p.currentTurnCompleted(fields)
	case "item/completed":
		return execution.InteractionUpdate{}, p.currentItemCompleted(fields, record)
	case "item/tool/requestUserInput":
		return p.currentRequestInput(nativeID, fields)
	case "serverRequest/resolved":
		return p.currentRequestResolved(fields)
	case "error":
		return execution.InteractionUpdate{}, p.currentError(fields)
	default:
		if len(nativeID) > 0 {
			return denyAppServerRequest(json.RawMessage(nativeID))
		}
		if strings.HasPrefix(method, "thread/") || strings.HasPrefix(method, "turn/") || strings.HasPrefix(method, "item/") || strings.HasPrefix(method, "serverRequest/") {
			return execution.InteractionUpdate{}, task.ErrInteractionRequest
		}
		return execution.InteractionUpdate{}, nil
	}
}

func (p *appServerProtocol) currentTurnStarted(fields map[string]providerjson.Value) (execution.InteractionUpdate, error) {
	params, err := currentParams(fields)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	selected, err := params.Object("threadId", "turn")
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	threadID, err := currentRequiredString(selected, "threadId", maxItemIDBytes)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	if threadID != p.threadID {
		return execution.InteractionUpdate{}, nil
	}
	turnValue, ok := selected["turn"]
	if !ok {
		return execution.InteractionUpdate{}, task.ErrInteractionConflict
	}
	turnFields, err := turnValue.Object("id")
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	turnID, err := currentRequiredString(turnFields, "id", maxItemIDBytes)
	if err != nil {
		return execution.InteractionUpdate{}, task.ErrInteractionConflict
	}
	if p.completed {
		return execution.InteractionUpdate{}, task.ErrInteractionClosed
	}
	return p.activateTurn(turnID)
}

func (p *appServerProtocol) currentTurnCompleted(fields map[string]providerjson.Value) (execution.InteractionUpdate, error) {
	params, err := currentParams(fields)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	selected, err := params.Object("threadId", "turn")
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	threadID, err := currentRequiredString(selected, "threadId", maxItemIDBytes)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	if threadID != p.threadID {
		return execution.InteractionUpdate{}, nil
	}
	turnValue, ok := selected["turn"]
	if !ok {
		return execution.InteractionUpdate{}, task.ErrInteractionConflict
	}
	turnFields, err := turnValue.Object("id", "status", "error")
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	turnID, err := currentRequiredString(turnFields, "id", maxItemIDBytes)
	if err != nil {
		return execution.InteractionUpdate{}, task.ErrInteractionConflict
	}
	status, err := currentRequiredString(turnFields, "status", maxItemTypeBytes)
	if err != nil {
		return execution.InteractionUpdate{}, task.ErrInteractionConflict
	}
	if p.turnID == "" || turnID != p.turnID || p.completed {
		return execution.InteractionUpdate{}, task.ErrInteractionConflict
	}
	failed, err := currentTurnFailedStatus(status, turnFields)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	p.completed = true
	p.failed = p.failed || failed
	return execution.InteractionUpdate{Done: true}, nil
}

func currentTurnFailedStatus(status string, fields map[string]providerjson.Value) (bool, error) {
	failed := status != "completed"
	if value, exists := fields["error"]; exists {
		null, err := currentValueIsNull(value)
		if err != nil {
			return false, err
		}
		failed = failed || !null
	}
	return failed, nil
}

func (p *appServerProtocol) currentItemCompleted(fields map[string]providerjson.Value, record providerjson.Value) error {
	params, err := currentParams(fields)
	if err != nil {
		return err
	}
	selected, err := params.Object("threadId", "turnId", "item")
	if err != nil {
		return err
	}
	threadID, err := currentRequiredString(selected, "threadId", maxItemIDBytes)
	if err != nil {
		return err
	}
	if threadID != p.threadID {
		return nil
	}
	turnID, err := currentRequiredString(selected, "turnId", maxItemIDBytes)
	if err != nil {
		return err
	}
	if turnID != p.turnID || p.completed {
		return task.ErrInteractionConflict
	}
	return p.currentCompletedAgentMessage(selected, record)
}

func (p *appServerProtocol) currentCompletedAgentMessage(selected map[string]providerjson.Value, record providerjson.Value) error {
	itemValue, ok := selected["item"]
	if !ok {
		return errors.New("codex app-server item is missing")
	}
	itemFields, err := itemValue.Object("type")
	if err != nil {
		return err
	}
	typeName, err := currentRequiredString(itemFields, "type", maxItemTypeBytes)
	if err != nil {
		return err
	}
	if typeName != "agentMessage" {
		return nil
	}
	itemFields, err = itemValue.Object("type", "phase", "id")
	if err != nil {
		return err
	}
	phase := ""
	if phaseValue, exists := itemFields["phase"]; exists {
		phase, err = phaseValue.String(maxItemTypeBytes)
		if err != nil {
			return err
		}
	}
	if err = p.recordCurrentAgentItem(itemFields); err != nil {
		return err
	}
	if phase == "commentary" {
		return nil
	}
	itemFields, err = itemValue.Object("type", "phase", "text")
	if err != nil {
		return err
	}
	textValue, ok := itemFields["text"]
	if !ok {
		return errors.New("codex final agent message text is missing")
	}
	nonblank, err := textValue.Nonblank()
	if err != nil {
		return err
	}
	p.finalStart = record.Start
	p.finalEnd = record.End
	p.finalSeen = true
	p.finalNonblank = nonblank
	return nil
}

func (p *appServerProtocol) recordCurrentAgentItem(itemFields map[string]providerjson.Value) error {
	if idValue, exists := itemFields["id"]; exists {
		id, idErr := idValue.String(maxItemIDBytes)
		if idErr != nil || id == "" {
			return task.ErrInteractionConflict
		}
		if len(p.agentItems) >= maxTrackedItems {
			if _, known := p.agentItems[id]; !known {
				return task.ErrInteractionRequest
			}
		}
		itemState := p.agentItems[id]
		if itemState.completed {
			return task.ErrInteractionConflict
		}
		itemState.completed = true
		p.agentItems[id] = itemState
	}
	return nil
}

func (p *appServerProtocol) currentRequestInput(nativeID []byte, fields map[string]providerjson.Value) (execution.InteractionUpdate, error) {
	params, err := currentParams(fields)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	selected, err := params.Object("threadId", "turnId", "isBlocking", "questions")
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	threadID, err := currentRequiredString(selected, "threadId", maxItemIDBytes)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	if threadID != p.threadID {
		return denyAppServerRequest(json.RawMessage(nativeID))
	}
	turnID, err := currentRequiredString(selected, "turnId", maxItemIDBytes)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	if p.completed || turnID != p.turnID || len(nativeID) == 0 || len(p.requests) >= maxTrackedItems {
		return execution.InteractionUpdate{}, task.ErrInteractionRequest
	}
	requestID := task.ComputeSHA256(nativeID)[:32]
	if _, exists := p.requests[requestID]; exists {
		return execution.InteractionUpdate{}, task.ErrInteractionConflict
	}
	blocking := false
	if blockingValue, exists := selected["isBlocking"]; exists {
		blocking, err = currentBool(blockingValue)
		if err != nil {
			return execution.InteractionUpdate{}, err
		}
	}
	questionsValue, ok := selected["questions"]
	if !ok {
		return execution.InteractionUpdate{}, task.ErrInteractionRequest
	}
	request := task.InteractionRequest{ID: requestID, TurnID: p.turnID, Blocking: blocking}
	request.Questions, err = currentQuestions(questionsValue)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	p.requests[requestID] = json.RawMessage(nativeID)
	return execution.InteractionUpdate{Events: []task.InteractionEvent{{Kind: "request", Request: &request}}}, nil
}

func currentQuestions(value providerjson.Value) ([]task.InteractionQuestion, error) {
	var questions []task.InteractionQuestion
	err := value.Array(func(value providerjson.Value) error {
		if len(questions) >= maxInteractionQuestions {
			return task.ErrInteractionRequest
		}
		question, err := currentQuestion(value)
		if err != nil {
			return err
		}
		questions = append(questions, question)
		return nil
	})
	return questions, err
}

func currentQuestion(value providerjson.Value) (task.InteractionQuestion, error) {
	fields, err := value.Object("id", "question", "isOther", "options")
	if err != nil {
		return task.InteractionQuestion{}, err
	}
	id, err := currentRequiredString(fields, "id", maxInteractionQuestionID)
	if err != nil || id == "" {
		return task.InteractionQuestion{}, task.ErrInteractionRequest
	}
	text, err := currentRequiredString(fields, "question", maxInteractionQuestionText)
	if err != nil {
		return task.InteractionQuestion{}, task.ErrInteractionRequest
	}
	isOther := false
	if otherValue, exists := fields["isOther"]; exists {
		isOther, err = currentBool(otherValue)
		if err != nil {
			return task.InteractionQuestion{}, task.ErrInteractionRequest
		}
	}
	question := task.InteractionQuestion{ID: id, Text: text, FreeText: isOther}
	if optionsValue, exists := fields["options"]; exists {
		question.Options, err = currentQuestionOptions(optionsValue)
		if err != nil {
			return task.InteractionQuestion{}, err
		}
	}
	question.FreeText = question.FreeText || len(question.Options) == 0
	return question, nil
}

func currentQuestionOptions(value providerjson.Value) ([]string, error) {
	var options []string
	err := value.Array(func(value providerjson.Value) error {
		if len(options) >= maxInteractionOptions {
			return task.ErrInteractionRequest
		}
		fields, err := value.Object("label")
		if err != nil {
			return err
		}
		label, err := currentRequiredString(fields, "label", maxInteractionOptionLabel)
		if err != nil || label == "" {
			return task.ErrInteractionRequest
		}
		options = append(options, label)
		return nil
	})
	return options, err
}

func (p *appServerProtocol) currentRequestResolved(fields map[string]providerjson.Value) (execution.InteractionUpdate, error) {
	params, err := currentParams(fields)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	selected, err := params.Object("threadId", "requestId")
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	threadID, err := currentRequiredString(selected, "threadId", maxItemIDBytes)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	if threadID != p.threadID {
		return execution.InteractionUpdate{}, nil
	}
	requestValue, ok := selected["requestId"]
	if !ok {
		return execution.InteractionUpdate{}, nil
	}
	nativeID, err := requestValue.Raw(maxInteractionNativeID)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	requestID := task.ComputeSHA256(nativeID)[:32]
	if _, exists := p.requests[requestID]; !exists {
		return execution.InteractionUpdate{}, nil
	}
	delete(p.requests, requestID)
	return execution.InteractionUpdate{Events: []task.InteractionEvent{{Kind: "resolved", RequestID: requestID}}}, nil
}

func (p *appServerProtocol) currentError(fields map[string]providerjson.Value) error {
	params, err := currentParams(fields)
	if err != nil {
		return err
	}
	selected, err := params.Object("threadId", "turnId", "willRetry", "error")
	if err != nil {
		return err
	}
	threadID, err := currentRequiredString(selected, "threadId", maxItemIDBytes)
	if err != nil {
		return err
	}
	turnID, err := currentRequiredString(selected, "turnId", maxItemIDBytes)
	if err != nil {
		return err
	}
	willRetryValue, retryOK := selected["willRetry"]
	errorValue, errorOK := selected["error"]
	if !retryOK || !errorOK {
		return task.ErrEvidenceFault
	}
	willRetry, err := currentBool(willRetryValue)
	if err != nil {
		return err
	}
	errorFields, err := errorValue.Object("message")
	if err != nil {
		return task.ErrEvidenceFault
	}
	if _, err = currentRequiredString(errorFields, "message", maxInteractionQuestionText); err != nil {
		return task.ErrEvidenceFault
	}
	if threadID != p.threadID {
		return nil
	}
	if turnID != p.turnID {
		return task.ErrIdentityMismatch
	}
	p.failed = p.failed || !willRetry
	return nil
}

func (p *appServerProtocol) currentSteerResponse(id string, fields map[string]providerjson.Value) (execution.InteractionUpdate, error) {
	messageID := strings.TrimPrefix(id, "delegate-steer-")
	if p.replay {
		return execution.InteractionUpdate{}, task.ValidateTaskID(messageID)
	}
	if !p.steering[messageID] {
		return execution.InteractionUpdate{}, task.ErrInteractionRequest
	}
	delete(p.steering, messageID)
	delivery := task.InteractionDelivery{MessageID: messageID, State: "acknowledged"}
	if errorValue, exists := fields["error"]; exists {
		null, err := currentValueIsNull(errorValue)
		if err != nil {
			return execution.InteractionUpdate{}, err
		}
		if !null {
			delivery.State = "rejected"
			delivery.Reason = "Codex rejected steering"
		}
	} else {
		resultValue, ok := fields["result"]
		if !ok {
			return execution.InteractionUpdate{}, task.ErrInteractionRequest
		}
		resultFields, err := resultValue.Object("turnId")
		if err != nil {
			return execution.InteractionUpdate{}, err
		}
		turnID, err := currentRequiredString(resultFields, "turnId", maxItemIDBytes)
		if err != nil || turnID != p.turnID {
			return execution.InteractionUpdate{}, task.ErrIdentityMismatch
		}
	}
	return execution.InteractionUpdate{Events: []task.InteractionEvent{{Kind: "delivery", Delivery: &delivery}}}, nil
}

func currentParams(fields map[string]providerjson.Value) (providerjson.Value, error) {
	params, ok := fields["params"]
	if !ok {
		return providerjson.Value{}, task.ErrInteractionRequest
	}
	return params, nil
}

func currentJSONIDString(raw []byte) (string, error) {
	var id string
	if err := json.Unmarshal(raw, &id); err != nil || id == "" {
		return "", task.ErrInteractionRequest
	}
	return id, nil
}

func currentBool(value providerjson.Value) (bool, error) {
	raw, err := value.Raw(32)
	if err != nil {
		return false, err
	}
	var decoded bool
	if err = json.Unmarshal(raw, &decoded); err != nil {
		return false, task.ErrInteractionRequest
	}
	return decoded, nil
}

func currentValueIsNull(value providerjson.Value) (bool, error) {
	position := value.Start
	for position < value.End {
		var one [1]byte
		n, err := value.Reader.ReadAt(one[:], position)
		if err != nil && !errors.Is(err, io.EOF) {
			return false, err
		}
		if n == 0 {
			return false, io.ErrUnexpectedEOF
		}
		if !strings.ContainsRune(" \t\r\n", rune(one[0])) {
			return one[0] == 'n', nil
		}
		position++
	}
	return false, io.ErrUnexpectedEOF
}

func readCurrentInteraction(raw predicate.Evidence, protocol *appServerProtocol) (error, error) {
	var semanticErr error
	stdoutErr := raw.Read(predicate.Stdout, func(reader io.Reader) error {
		var readErr error
		semanticErr, readErr = providerjson.ReadRecords(reader, func(value providerjson.Value, _ uint64) error {
			_, receiveErr := protocol.ReceiveRecord(value)
			if receiveErr != nil {
				return receiveErr
			}
			return nil
		})
		return readErr
	})
	stderrErr := raw.Read(predicate.Stderr, commonprovider.DrainReader)
	return errors.Join(stdoutErr, stderrErr), semanticErr
}

func currentAppServerFinalText(value providerjson.Value) (providerjson.Value, error) {
	top, err := value.Object("method", "params")
	if err != nil {
		return providerjson.Value{}, err
	}
	method, err := currentRequiredString(top, "method", maxItemTypeBytes)
	if err != nil || method != "item/completed" {
		return providerjson.Value{}, errors.New("selected Codex record is not item/completed")
	}
	params, err := currentParams(top)
	if err != nil {
		return providerjson.Value{}, err
	}
	selected, err := params.Object("item")
	if err != nil {
		return providerjson.Value{}, err
	}
	itemValue, ok := selected["item"]
	if !ok {
		return providerjson.Value{}, errors.New("selected Codex item is missing")
	}
	itemFields, err := itemValue.Object("type", "phase")
	if err != nil {
		return providerjson.Value{}, err
	}
	typeName, err := currentRequiredString(itemFields, "type", maxItemTypeBytes)
	if err != nil || typeName != "agentMessage" {
		return providerjson.Value{}, errors.New("selected Codex item is not an agent message")
	}
	if phaseValue, exists := itemFields["phase"]; exists {
		phase, phaseErr := phaseValue.String(maxItemTypeBytes)
		if phaseErr != nil || phase == "commentary" {
			return providerjson.Value{}, errors.New("selected Codex item is not a final agent message")
		}
	}
	itemFields, err = itemValue.Object("type", "phase", "text")
	if err != nil {
		return providerjson.Value{}, err
	}
	textValue, ok := itemFields["text"]
	if !ok {
		return providerjson.Value{}, errors.New("selected Codex final text is missing")
	}
	return textValue, nil
}

func streamCurrentAppServerFinal(raw predicate.Evidence, start, end int64, out io.Writer) (currentDigest, error) {
	digest := newCurrentDigest()
	writer := &currentOutputWriter{digest: digest, out: out}
	var found bool
	var semanticErr, readErr error
	readErr = raw.Read(predicate.Stdout, func(reader io.Reader) error {
		semanticErr, readErr = providerjson.ReadRecords(reader, func(value providerjson.Value, _ uint64) error {
			if value.Start != start || value.End != end {
				return nil
			}
			found = true
			textValue, err := currentAppServerFinalText(value)
			if err != nil {
				return err
			}
			return textValue.CopyString(writer)
		})
		return readErr
	})
	if readErr != nil {
		return currentDigest{}, readErr
	}
	if semanticErr != nil {
		var writeErr *currentAnswerWriteError
		if errors.As(semanticErr, &writeErr) {
			return currentDigest{}, writeErr.err
		}
		return currentDigest{}, fmt.Errorf("streaming current Codex interaction answer: %w", semanticErr)
	}
	if !found {
		return currentDigest{}, errors.New("selected Codex interaction answer record was not found")
	}
	return *digest, nil
}

var (
	_ execution.CurrentRecordInteractionProtocol = (*appServerProtocol)(nil)
	_ predicate.RecoveryInterpreter              = interactionInterpreter{}
)
