package pi

import (
	"bytes"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/providerjson"
)

const (
	eventSessionInfoChanged   = "session_info_changed"
	eventThinkingLevelChanged = "thinking_level_changed"
)

// piCurrentSelectedFields names the control fields the current parser reads
// for one event type. Fields selected only under the native-outcome policy
// stay unselected for earlier revisions so their replay is unchanged.
func piCurrentSelectedFields(typeName string, nativeOutcome bool) []string {
	switch typeName {
	case eventSession:
		return []string{"id", "version", "cwd"}
	case eventMessageStart, eventMessageEnd:
		return []string{"message"}
	case eventMessageUpdate:
		return []string{"assistantMessageEvent"}
	case eventTurnEnd:
		return []string{"message", "toolResults"}
	case eventAgentEnd:
		return []string{"messages"}
	case eventError:
		return []string{"error"}
	case eventAgentSettled:
		if nativeOutcome {
			return []string{"aborted"}
		}
	case eventAutoRetryEnd:
		if nativeOutcome {
			return []string{"success"}
		}
	}
	return nil
}

// piCurrentObservationalEvent is the single classifier for the documented Pi
// events that report display or configuration changes. They start no work and
// carry no completion authority, so the native-outcome policy ignores them
// wherever they appear, including after agent_settled.
func piCurrentObservationalEvent(typeName string) bool {
	return typeName == eventSessionInfoChanged || typeName == eventThinkingLevelChanged
}

// applyPiCurrentSettledOutcome records an explicit native abort. An absent
// field keeps the older native shape; a present field must be a boolean.
func applyPiCurrentSettledOutcome(state *eventState, fields map[string]providerjson.Value) {
	if !state.nativeOutcome {
		return
	}
	aborted, present, err := piCurrentOptionalBool(fields, "aborted")
	if err != nil {
		state.markSemantic("agent_settled aborted is not a boolean")
		return
	}
	if present && aborted {
		state.nativeOutcomeFailed = true
	}
}

// applyPiCurrentRetryOutcome records Pi's documented final retry failure for
// the current cycle. auto_retry_end.success=true is telemetry and never
// establishes completion; the cycle's own messages remain the authority.
func applyPiCurrentRetryOutcome(state *eventState, fields map[string]providerjson.Value) {
	if !state.nativeOutcome {
		return
	}
	success, present, err := piCurrentOptionalBool(fields, "success")
	if err != nil {
		state.markSemantic("auto_retry_end success is not a boolean")
		return
	}
	if present && !success {
		state.nativeOutcomeFailed = true
	}
}

func piCurrentOptionalBool(fields map[string]providerjson.Value, name string) (value, present bool, err error) {
	field, ok := fields[name]
	if !ok {
		return false, false, nil
	}
	raw, err := field.Raw(8)
	if err != nil {
		return false, true, err
	}
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, true, nil
	case "false":
		return false, true, nil
	default:
		return false, true, errors.New(name + " is not a boolean")
	}
}
