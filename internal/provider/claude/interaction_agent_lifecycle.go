package claude

import (
	"encoding/json"

	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

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
