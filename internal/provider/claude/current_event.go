package claude

import (
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/providerjson"
)

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

// claudeMessageScope is the typed owner of one complete assistant or user
// message, decoded once from parent_tool_use_id. Claude stamps a nested
// agent's messages with the opaque id of the tool call that spawned the agent
// and leaves root messages with null or no field. The id is never retained or
// compared: only the scope class travels with the event. Frozen completion
// policies ignore the class; the scoped policy routes and rejects by it.
type claudeMessageScope uint8

const (
	// claudeMessageScopeRoot covers an absent or null parent_tool_use_id and
	// every event that is not a complete assistant or user message.
	claudeMessageScopeRoot claudeMessageScope = iota
	// claudeMessageScopeNested is a nonblank, valid-text string of at most
	// maxParentToolUseIDBytes bytes.
	claudeMessageScopeNested
	// claudeMessageScopeInvalid is any other parent_tool_use_id value.
	claudeMessageScopeInvalid
)

const (
	parentToolUseIDField    = "parent_tool_use_id"
	maxParentToolUseIDBytes = 1024
)

type claudeCurrentEvent struct {
	typeName string
	fields   map[string]providerjson.Value
	value    providerjson.Value
	scope    claudeMessageScope
}

// claudeCurrentEventFields selects the top-level fields of one event. The advisory
// projection selects only the authority and the controls of a system event's
// own subtype, so an ambiguous advisory or unconsumed field cannot reject it.
func claudeCurrentEventFields(value providerjson.Value, typeName string, advisory bool) (map[string]providerjson.Value, claudeMessageScope, error) {
	names := []string{"session_id"}
	message := false
	switch typeName {
	case "system":
		if advisory {
			selected, err := claudeAdvisorySystemFields(value)
			if err != nil {
				return nil, claudeMessageScopeRoot, err
			}
			names = selected
		} else {
			names = append(names, "subtype", "claude_code_version", "permissionMode", "cwd", "apiKeySource", "tools", "mcp_servers", "model", "state", "task_type", "task_id", "patch")
		}
	case "result":
		names = append(names, "subtype", "is_error", "result", "stop_reason", "errors", "permission_denials")
	case "assistant", "user":
		names = append(names, "message")
		message = true
	case "control_response":
		names = append(names, "response")
	case "control_request":
		names = append(names, "request_id", "request")
	case "control_cancel_request":
		names = append(names, "request_id")
	}
	if !message {
		fields, err := value.Object(names...)
		return fields, claudeMessageScopeRoot, err
	}
	fields, err := value.Object(append(names, parentToolUseIDField)...)
	if err != nil {
		// An ambiguous scope field (for example a case alias) must not reject
		// frozen policies, which never read it. Retry without it and label the
		// message invalid; any other fault repeats and is returned.
		fields, err = value.Object(names...)
		return fields, claudeMessageScopeInvalid, err
	}
	return fields, decodeClaudeMessageScope(fields), nil
}

func decodeClaudeMessageScope(fields map[string]providerjson.Value) claudeMessageScope {
	parent, present := fields[parentToolUseIDField]
	if !present || claudeCurrentValueIsNull(parent) {
		return claudeMessageScopeRoot
	}
	id, err := parent.String(maxParentToolUseIDBytes)
	if err != nil || !validText(id) {
		return claudeMessageScopeInvalid
	}
	return claudeMessageScopeNested
}

// decodeClaudeCurrentEvent is the frozen strict projection: every key of the
// whole record is unique, including case-folded aliases, at every level.
func decodeClaudeCurrentEvent(value providerjson.Value) (claudeCurrentEvent, error) {
	if err := value.ValidateUniqueKeys(); err != nil {
		return claudeCurrentEvent{}, err
	}
	typeName, err := claudeCurrentEventType(value)
	if err != nil {
		return claudeCurrentEvent{}, err
	}
	return projectClaudeCurrentEvent(value, typeName, false)
}

func projectClaudeCurrentEvent(value providerjson.Value, typeName string, advisory bool) (claudeCurrentEvent, error) {
	fields, scope, err := claudeCurrentEventFields(value, typeName, advisory)
	if err != nil {
		return claudeCurrentEvent{}, err
	}
	return claudeCurrentEvent{typeName: typeName, fields: fields, value: value, scope: scope}, nil
}
