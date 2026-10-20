package opencode

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// response handles the reply to the single outstanding request. A response
// to anything else, including a second response to an answered request, is a
// protocol fault. A native JSON-RPC error ends the turn and closes input.
func (s *acpSession) response(message acpMessage) (execution.InteractionUpdate, error) {
	id, err := acpResponseID(message.id)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	if s.pending == 0 || id != s.pending {
		return execution.InteractionUpdate{}, fmt.Errorf("%w: response %d does not match the outstanding request", errACPProtocol, id)
	}
	if message.failed {
		code, codeErr := acpErrorCode(message.err)
		if codeErr != nil {
			return execution.InteractionUpdate{}, codeErr
		}
		return s.finish(acpTerminalNativeError, fmt.Sprintf("%s request %d returned JSON-RPC error %d", s.method, id, code)), nil
	}
	switch s.phase {
	case acpPhaseInitializing:
		return s.initialized(message.result)
	case acpPhaseOpening:
		return s.opened(message.result)
	case acpPhaseConfiguring:
		return s.configured(message.result)
	case acpPhasePrompting:
		return s.prompted(message.result)
	case acpPhaseNew, acpPhaseTerminal:
		return execution.InteractionUpdate{}, fmt.Errorf("%w: no outstanding request", errACPProtocol)
	default:
		return execution.InteractionUpdate{}, fmt.Errorf("%w: unknown phase", errACPProtocol)
	}
}

// initialized validates protocol negotiation and, for a continuation, the
// native session/resume capability, then opens or resumes the session.
// session/resume attaches the exact recorded session without replaying its
// history as updates.
func (s *acpSession) initialized(result providerjson.Value) (execution.InteractionUpdate, error) {
	fields, err := result.Object("protocolVersion", "agentCapabilities")
	if err != nil {
		return execution.InteractionUpdate{}, fmt.Errorf("%w: initialize result: %w", errACPProtocol, err)
	}
	version, ok := fields["protocolVersion"]
	if !ok {
		return execution.InteractionUpdate{}, fmt.Errorf("%w: initialize result has no protocolVersion", errACPProtocol)
	}
	if raw, rawErr := version.Raw(16); rawErr != nil || string(raw) != strconv.Itoa(acpProtocolVersion) {
		return execution.InteractionUpdate{}, fmt.Errorf("%w: native agent did not negotiate ACP protocol version %d", errACPProtocol, acpProtocolVersion)
	}
	if s.expected == "" {
		return acpSend(s, acpPhaseOpening, acpMethodSessionNew, acpNewSessionParams{Cwd: s.outboundCwd()})
	}
	capabilities, err := acpRequiredObject(fields, "agentCapabilities", "sessionCapabilities")
	if err == nil {
		capabilities, err = acpRequiredObject(capabilities, "sessionCapabilities", "resume")
	}
	if err == nil {
		_, err = acpRequiredObject(capabilities, "resume")
	}
	if err != nil {
		return execution.InteractionUpdate{}, fmt.Errorf("native agent does not advertise session/resume: %w", err)
	}
	return acpSend(s, acpPhaseOpening, acpMethodSessionResume, acpResumeSessionParams{SessionID: s.expected, Cwd: s.outboundCwd()})
}

func (s *acpSession) outboundCwd() string {
	if s.outbound == nil {
		return ""
	}
	return s.outbound.cwd
}

// opened binds the exact native session and records it once through the
// core-owned callback. A resume response may omit the identifier; one that
// names it must name the expected session.
func (s *acpSession) opened(result providerjson.Value) (execution.InteractionUpdate, error) {
	fields, err := result.Object("sessionId")
	if err != nil {
		return execution.InteractionUpdate{}, fmt.Errorf("%w: session result: %w", errACPProtocol, err)
	}
	id := s.expected
	if _, present := fields["sessionId"]; present || id == "" {
		if id, err = acpSessionID(fields, "sessionId"); err != nil {
			return execution.InteractionUpdate{}, err
		}
	}
	if s.expected != "" && id != s.expected {
		return execution.InteractionUpdate{}, fmt.Errorf("%w: resumed OpenCode session differs from the recorded session", task.ErrIdentityMismatch)
	}
	s.session = id
	if s.outbound != nil {
		if err = s.outbound.record(task.SessionIdentity{Provider: Provider, ConversationID: id}); err != nil {
			return execution.InteractionUpdate{}, fmt.Errorf("recording OpenCode session identity: %w", err)
		}
	}
	return s.advance()
}

// advance sends the next configuration option, or the prompt once every
// option is acknowledged.
func (s *acpSession) advance() (execution.InteractionUpdate, error) {
	if s.applied < len(s.settings) {
		setting := s.settings[s.applied]
		return acpSend(s, acpPhaseConfiguring, acpMethodSetConfigOption, acpSetConfigOptionParams{SessionID: s.session, ConfigID: setting.id, Value: setting.value})
	}
	var prompt json.RawMessage
	if s.outbound != nil {
		prompt = s.outbound.prompt
	}
	return acpSend(s, acpPhasePrompting, acpMethodSessionPrompt, acpPromptParams{SessionID: s.session, Prompt: prompt})
}

// configured requires the native acknowledgement of the option just set and
// of every option set before it, so a later change cannot silently revert an
// earlier one. A missing or different value refuses before the prompt.
func (s *acpSession) configured(result providerjson.Value) (execution.InteractionUpdate, error) {
	fields, err := result.Object("configOptions")
	if err != nil {
		return execution.InteractionUpdate{}, fmt.Errorf("%w: set_config_option result: %w", errACPProtocol, err)
	}
	options, ok := fields["configOptions"]
	if !ok {
		return execution.InteractionUpdate{}, fmt.Errorf("%w: set_config_option result has no configOptions", errACPProtocol)
	}
	applied := s.settings[:s.applied+1]
	acknowledged := make([]bool, len(applied))
	seen := make([]bool, len(applied))
	err = options.Array(func(option providerjson.Value) error {
		return acknowledgeACPOption(option, applied, acknowledged, seen)
	})
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	for index, ok := range acknowledged {
		if !ok {
			return s.finish(acpTerminalUnacknowledged, fmt.Sprintf("native config option %q did not acknowledge the requested value", applied[index].id)), nil
		}
	}
	s.applied++
	return s.advance()
}

func acknowledgeACPOption(option providerjson.Value, applied []acpSetting, acknowledged, seen []bool) error {
	fields, err := option.Object("id", "currentValue")
	if err != nil {
		return fmt.Errorf("%w: config option: %w", errACPProtocol, err)
	}
	id, err := acpRequiredString(fields, "id", maxACPControlBytes)
	if err != nil {
		return err
	}
	for index, setting := range applied {
		if setting.id != id {
			continue
		}
		if seen[index] {
			return fmt.Errorf("%w: config option %q is reported twice", errACPProtocol, id)
		}
		seen[index] = true
		current, currentErr := acpRequiredString(fields, "currentValue", maxACPControlBytes)
		if currentErr != nil {
			return currentErr
		}
		acknowledged[index] = current == setting.value
	}
	return nil
}

// prompted applies the completion rule: only stopReason end_turn completes.
// Usage is optional advisory accounting and never decides completion.
func (s *acpSession) prompted(result providerjson.Value) (execution.InteractionUpdate, error) {
	fields, err := result.Object("stopReason", "usage")
	if err != nil {
		return execution.InteractionUpdate{}, fmt.Errorf("%w: prompt result: %w", errACPProtocol, err)
	}
	reason, err := acpStopReason(fields)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	if usage, present := fields["usage"]; present {
		s.usage = parseACPUsage(usage)
	}
	if reason != "end_turn" {
		s.stopReason = reason
		return s.finish(acpTerminalStopped, "stopReason "+reason), nil
	}
	return s.finish(acpTerminalEndTurn, ""), nil
}

// notification validates every session/update against the task session.
// Root and child updates both carry the root session ID; child updates are
// tagged and excluded from the answer. Only root agent_message_chunk text
// during the prompt forms the answer. Other notification methods and update
// kinds carry no completion authority and remain in raw stdout.
func (s *acpSession) notification(message acpMessage) error {
	if message.method != acpMethodSessionUpdate {
		return nil
	}
	fields, err := message.params.Object("sessionId", "update")
	if err != nil {
		return fmt.Errorf("%w: session/update params: %w", errACPProtocol, err)
	}
	id, err := acpSessionID(fields, "sessionId")
	if err != nil {
		return err
	}
	if err = requireTaskSession(s.session, id); err != nil {
		return err
	}
	update, err := acpRequiredObject(fields, "update", "sessionUpdate", "content", "_meta")
	if err != nil {
		return err
	}
	kind, err := acpRequiredString(update, "sessionUpdate", maxACPControlBytes)
	if err != nil || kind != "agent_message_chunk" {
		return err
	}
	child, err := acpChildTagged(update)
	if err != nil || child {
		return err
	}
	return s.rootText(update)
}

// rootText streams one root answer chunk. Root answer text outside the active
// prompt cannot belong to this task's single turn.
func (s *acpSession) rootText(update map[string]providerjson.Value) error {
	if s.phase != acpPhasePrompting {
		return fmt.Errorf("%w: root answer text outside the task prompt", errACPProtocol)
	}
	content, err := acpRequiredObject(update, "content", "type", "text")
	if err != nil {
		return err
	}
	kind, err := acpRequiredString(content, "type", maxACPControlBytes)
	if err != nil || kind != "text" {
		return err
	}
	text, ok := content["text"]
	if !ok {
		return fmt.Errorf("%w: text content has no text", errACPProtocol)
	}
	return s.answer.add(text)
}

// agentRequest answers a native request while the turn is open. Only
// permission requests are supported; filesystem, terminal and elicitation
// capabilities were not advertised, so any other method receives "method not
// found". After the terminal response no reply can be sent.
func (s *acpSession) agentRequest(message acpMessage) (execution.InteractionUpdate, error) {
	id, err := acpRequestID(message.id)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	if s.phase == acpPhaseTerminal {
		return execution.InteractionUpdate{}, fmt.Errorf("%w: native request after the terminal response", errACPProtocol)
	}
	if message.method == acpMethodPermission {
		return s.permission(id, message.params)
	}
	return s.reply(func() ([]byte, error) { return encodeACPMethodNotFound(id) })
}

// permission applies the deterministic mode policy: workspace-write selects
// the offered allow_once option, read-only selects reject_once. Read-only
// without a reject option cancels the request; workspace-write without
// allow_once is a protocol fault.
func (s *acpSession) permission(id json.RawMessage, params providerjson.Value) (execution.InteractionUpdate, error) {
	if s.phase != acpPhasePrompting {
		return execution.InteractionUpdate{}, fmt.Errorf("%w: permission request outside the task prompt", errACPProtocol)
	}
	fields, err := params.Object("sessionId", "toolCall", "options")
	if err != nil {
		return execution.InteractionUpdate{}, fmt.Errorf("%w: permission params: %w", errACPProtocol, err)
	}
	session, err := acpSessionID(fields, "sessionId")
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	if err = requireTaskSession(s.session, session); err != nil {
		return execution.InteractionUpdate{}, err
	}
	if _, err = acpRequiredObject(fields, "toolCall"); err != nil {
		return execution.InteractionUpdate{}, err
	}
	options, ok := fields["options"]
	if !ok {
		return execution.InteractionUpdate{}, fmt.Errorf("%w: permission request has no options", errACPProtocol)
	}
	kind := permissionRejectOnce
	if s.mode == ModeWorkspaceWrite {
		kind = permissionAllowOnce
	}
	optionID, err := acpPermissionOption(options, kind)
	if err != nil {
		return execution.InteractionUpdate{}, err
	}
	outcome := acpPermissionOutcome{Outcome: "selected", OptionID: optionID}
	if optionID == "" {
		if kind == permissionAllowOnce {
			return execution.InteractionUpdate{}, fmt.Errorf("%w: workspace-write permission request offers no allow_once option", errACPProtocol)
		}
		outcome = acpPermissionOutcome{Outcome: "cancelled"}
	}
	return s.reply(func() ([]byte, error) { return encodeACPPermissionReply(id, outcome) })
}
