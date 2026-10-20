package opencode

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

var (
	// errACPIncomplete marks a stream that ended without the terminal response
	// to the task's single session/prompt request.
	errACPIncomplete = errors.New("OpenCode ACP turn did not complete")
	// errACPTurnFailed marks a terminal native result other than end_turn: a
	// JSON-RPC error, an unacknowledged configuration, or another stop reason.
	errACPTurnFailed = errors.New("OpenCode ACP turn did not end successfully")
)

// acpPhase names the single outstanding adapter request. The adapter sends
// one request at a time, so every response matches exactly one ID.
type acpPhase int

const (
	acpPhaseNew acpPhase = iota
	acpPhaseInitializing
	acpPhaseOpening
	acpPhaseConfiguring
	acpPhasePrompting
	acpPhaseTerminal
)

// acpTerminal classifies how the native turn ended.
type acpTerminal int

const (
	acpTerminalNone acpTerminal = iota
	acpTerminalEndTurn
	acpTerminalStopped
	acpTerminalNativeError
	acpTerminalUnacknowledged
)

// acpSetting is one native session configuration option, applied in order.
type acpSetting struct {
	id    string
	value string
}

// acpOutbound encodes the live session's requests. A replay has none: the
// sealed stream carries only native output, and the deterministic request
// sequence is reconstructed from the immutable request.
type acpOutbound struct {
	cwd    string
	prompt json.RawMessage
	record func(task.SessionIdentity) error
}

// acpSession is the single reducer over one ACP stdout stream. The live
// protocol and the sealed-evidence interpreter feed it the same records in
// the same order, so both reach the same state. It retains only bounded
// identifiers, its phase, the terminal classification, optional usage and
// the answer accumulator; it never retains frame bytes or answer text.
type acpSession struct {
	mode     string
	expected string
	settings []acpSetting
	outbound *acpOutbound
	session  string
	method   string
	pending  int64
	nextID   int64
	applied  int
	phase    acpPhase
	terminal acpTerminal
	// stopReason is the validated native token of a non-end_turn response.
	stopReason string
	detail     string
	fault      error
	answer     acpAnswer
	usage      acpUsage
}

// acpSettings returns the native options set before the prompt: the agent for
// the permission mode, then an explicitly requested model and effort,
// unchanged. Model precedes effort because OpenCode's effort option belongs
// to the current model. An omitted or "default" effort sends nothing.
func acpSettings(mode string, requested task.TaskConfig) ([]acpSetting, error) {
	agent, err := nativeAgent(mode)
	if err != nil {
		return nil, err
	}
	settings := []acpSetting{{id: acpConfigMode, value: agent}}
	if requested.Model != "" {
		settings = append(settings, acpSetting{id: acpConfigModel, value: requested.Model})
	}
	if explicitEffort(requested.Effort) {
		settings = append(settings, acpSetting{id: acpConfigEffort, value: requested.Effort})
	}
	return settings, nil
}

// newACPSession validates the session expectation for mode and settings.
func newACPSession(mode string, requested task.TaskConfig, expected task.SessionExpectation, outbound *acpOutbound) (*acpSession, error) {
	if (expected.Required && !validSessionID(expected.ID)) || (!expected.Required && expected.ID != "") {
		return nil, task.ErrIdentityMismatch
	}
	settings, err := acpSettings(mode, requested)
	if err != nil {
		return nil, err
	}
	return &acpSession{mode: mode, expected: expected.ID, settings: settings, outbound: outbound, nextID: 1}, nil
}

// begin sends initialize as request 1. A live session has validated and
// encoded its prompt before this point.
func (s *acpSession) begin() ([][]byte, error) {
	if s.phase != acpPhaseNew {
		return nil, fmt.Errorf("%w: protocol already started", errACPProtocol)
	}
	update, err := acpSend(s, acpPhaseInitializing, acpMethodInitialize, acpInitializeParams{
		ProtocolVersion: acpProtocolVersion, ClientInfo: acpImplementation{Name: acpClientName, Version: acpClientVersion},
	})
	return update.Outbound, err
}

// receive consumes one stdout record. A fault is sticky: no later record can
// complete the turn, and the live engine stops consuming at the first one.
func (s *acpSession) receive(value providerjson.Value) (execution.InteractionUpdate, error) {
	if s.fault != nil {
		return execution.InteractionUpdate{}, s.fault
	}
	update, err := s.dispatch(value)
	if err != nil {
		s.fault = err
	}
	return update, err
}

func (s *acpSession) dispatch(value providerjson.Value) (execution.InteractionUpdate, error) {
	message, err := decodeACPMessage(value)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	switch message.kind {
	case acpRequest:
		return s.agentRequest(message)
	case acpNotification:
		return execution.InteractionUpdate{}, s.notification(message)
	case acpResponse:
		return s.response(message)
	default:
		return execution.InteractionUpdate{}, fmt.Errorf("%w: unknown frame kind", errACPProtocol)
	}
}

// completion is the protocol verdict. It is stable after the stream ends.
func (s *acpSession) completion() error {
	if s.fault != nil {
		return s.fault
	}
	switch s.terminal {
	case acpTerminalNone:
		return fmt.Errorf("%w: no response to the task prompt was observed", errACPIncomplete)
	case acpTerminalEndTurn:
		return nil
	case acpTerminalStopped, acpTerminalNativeError, acpTerminalUnacknowledged:
		return fmt.Errorf("%w: %s", errACPTurnFailed, s.detail)
	default:
		return fmt.Errorf("%w: unknown terminal state", errACPProtocol)
	}
}

// acpSend encodes the next deterministic request and makes it the only
// outstanding one. Request IDs are 1, 2, 3... in send order. A replay
// advances identically but encodes nothing.
func acpSend[P any](s *acpSession, next acpPhase, method string, params P) (execution.InteractionUpdate, error) {
	id := s.nextID
	s.pending, s.method, s.nextID, s.phase = id, method, id+1, next
	if s.outbound == nil {
		return execution.InteractionUpdate{}, nil
	}
	frame, err := encodeACPRequest(id, method, params)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	return execution.InteractionUpdate{Outbound: [][]byte{frame}}, nil
}

// reply returns one encoded reply to a native request; a replay sends none.
func (s *acpSession) reply(encode func() ([]byte, error)) (execution.InteractionUpdate, error) {
	if s.outbound == nil {
		return execution.InteractionUpdate{}, nil
	}
	frame, err := encode()
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	return execution.InteractionUpdate{Outbound: [][]byte{frame}}, nil
}

// finish records the terminal classification and ends native input. No
// request is outstanding afterward, so any later response is a duplicate.
func (s *acpSession) finish(terminal acpTerminal, detail string) execution.InteractionUpdate {
	s.phase, s.terminal, s.detail, s.pending, s.method = acpPhaseTerminal, terminal, detail, 0, ""
	if s.outbound != nil {
		s.outbound.prompt = nil
	}
	return execution.InteractionUpdate{Done: true}
}
