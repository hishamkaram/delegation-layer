package claude

import (
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/providerjson"
)

// The completion policy owns the wire projection too: event selection, init
// decoding, and repeated-init comparison change together under one immutable
// predicate identity. Ordinary print, CCR, interactive live decoding, and
// sealed replay all reach them through the reducer of their admitted policy;
// frozen policies keep the strict projection they were sealed under.

// claudeAdvisorySystemControls are the fields a system event of subtype
// selects under the advisory projection, beyond session_id and subtype: the
// init permission authority or the lifecycle controls the interactive closure
// observer reads for that subtype. Any other field, including another
// subtype's control and advisory init metadata, is never selected, so its
// duplicates cannot reject the event.
func claudeAdvisorySystemControls(subtype string) []string {
	switch subtype {
	case "init":
		return []string{"permissionMode"}
	case "session_state_changed":
		return []string{"state"}
	case "task_started":
		return []string{"task_type", "task_id"}
	case "task_notification":
		return []string{"task_id"}
	case "task_updated":
		return []string{"task_id", "patch"}
	default:
		return nil
	}
}

// claudeAdvisorySystemFields selects the subtype first and then only the
// controls of that subtype. A missing or invalid subtype selects nothing
// more; every consumer re-reads the selected subtype and rejects it.
func claudeAdvisorySystemFields(value providerjson.Value) ([]string, error) {
	head, err := value.Object("session_id", "subtype")
	if err != nil {
		return nil, err
	}
	names := []string{"session_id", "subtype"}
	if subtype, present, subtypeErr := claudeCurrentOptionalString(head, "subtype", 256); subtypeErr == nil && present {
		names = append(names, claudeAdvisorySystemControls(subtype)...)
	}
	return names, nil
}

// advisoryWire reports whether the immutable policy reads the advisory wire.
func (a *completionAttribution) advisoryWire() bool { return a != nil && a.advisory }

// decodeEvent is the one record projection of the immutable policy.
func (a *completionAttribution) decodeEvent(value providerjson.Value) (claudeCurrentEvent, error) {
	if a.advisoryWire() {
		return decodeClaudeAdvisoryEvent(value)
	}
	return decodeClaudeCurrentEvent(value)
}

// decodeClaudeAdvisoryEvent validates the complete record syntax while
// selecting its type and rejects duplicate selected fields, including
// case-folded aliases, at every level the policy reads. Opaque tool, content,
// and extension payloads are not checked for unique keys. Host control
// records stay strict as a whole: the interactive host consumes them,
// including a question input it echoes back to the provider.
func decodeClaudeAdvisoryEvent(value providerjson.Value) (claudeCurrentEvent, error) {
	typeName, err := claudeCurrentEventType(value)
	if err != nil {
		return claudeCurrentEvent{}, err
	}
	if isClaudeHostControlEvent(typeName) {
		if err = value.ValidateUniqueKeys(); err != nil {
			return claudeCurrentEvent{}, err
		}
	}
	event, err := projectClaudeCurrentEvent(value, typeName, true)
	if err != nil {
		return claudeCurrentEvent{}, err
	}
	selectClaudeAdvisoryInitModel(event)
	return event, nil
}

func isClaudeHostControlEvent(typeName string) bool {
	switch typeName {
	case "control_request", "control_response", "control_cancel_request":
		return true
	default:
		return false
	}
}

// selectClaudeAdvisoryInitModel adds the init model, the only advisory init
// field with a consumer, when its key is unambiguous. An ambiguous key is
// dropped like any other malformed advisory field.
func selectClaudeAdvisoryInitModel(event claudeCurrentEvent) {
	if !isClaudeInitEvent(event) {
		return
	}
	selected, err := event.value.Object("model")
	if err != nil {
		return
	}
	if model, present := selected["model"]; present {
		event.fields["model"] = model
	}
}

// decodeInit decodes one system/init under the immutable policy. The advisory
// policy reads only the session and the requested native permission as
// authority and retains a valid model name for usage; every other init field
// is advisory and never defaulted.
func (a *completionAttribution) decodeInit(fields map[string]providerjson.Value, mode string) (initState, error) {
	if !a.advisoryWire() {
		return decodeClaudeCurrentInit(fields, mode)
	}
	sessionID, err := decodeClaudeInitSession(fields)
	if err != nil {
		return initState{}, err
	}
	permission, err := decodeClaudeInitPermission(fields, mode)
	if err != nil {
		return initState{}, err
	}
	return initState{sessionID: sessionID, permission: permission, model: advisoryClaudeInitModel(fields)}, nil
}

// sameInit compares a repeated system/init with the initial one: the advisory
// policy compares its authority only, the frozen policies every field.
func (a *completionAttribution) sameInit(left, right initState) bool {
	if a.advisoryWire() {
		return left.sessionID == right.sessionID && left.permission == right.permission
	}
	return sameClaudeInit(left, right)
}

func decodeClaudeInitSession(fields map[string]providerjson.Value) (string, error) {
	sessionID, err := claudeCurrentRequiredString(fields, "session_id", 128)
	if err != nil || !validSessionID(sessionID) {
		return "", errors.New("system/init session_id is not a UUID")
	}
	return sessionID, nil
}

func decodeClaudeInitPermission(fields map[string]providerjson.Value, mode string) (string, error) {
	permission, err := claudeCurrentRequiredText(fields, "permissionMode", 128)
	if err != nil {
		return "", err
	}
	wantPermission := "plan"
	if mode == WorkspaceWriteMode {
		wantPermission = "bypassPermissions"
	} else if mode != Mode {
		return "", fmt.Errorf("unsupported Claude permission mode %q", mode)
	}
	if permission != wantPermission {
		return "", fmt.Errorf("system/init permissionMode is not %s", wantPermission)
	}
	return permission, nil
}

// advisoryClaudeInitModel returns a valid model name, or nothing for a
// missing or malformed advisory value.
func advisoryClaudeInitModel(fields map[string]providerjson.Value) string {
	model, present, err := claudeCurrentOptionalString(fields, "model", maxModelNameBytes)
	if err != nil || !present || !validModelName(model) {
		return ""
	}
	return model
}
