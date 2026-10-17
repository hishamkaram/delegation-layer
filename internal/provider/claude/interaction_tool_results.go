package claude

import (
	"encoding/json"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

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
