package claude

import (
	"errors"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/providerjson"
)

const (
	claudeOriginHuman            = "human"
	claudeOriginTaskNotification = "task-notification"
	claudeTerminalCompleted      = "completed"
)

// claudeFailedTerminalReasons are the documented query-loop endings other
// than completion. Any other terminal_reason value is undocumented evidence.
var claudeFailedTerminalReasons = map[string]struct{}{
	"max_turns":         {},
	"api_error":         {},
	"aborted_streaming": {},
	"aborted_tools":     {},
}

type claudeResultOrigin struct {
	present        bool
	kind           string
	subkindPresent bool
}

// claudeResultCompletion is the typed completion metadata of one current
// result record, consumed only by the completion reducer. Absent fields keep
// older CLI releases compatible; an explicit null or a wrongly typed value is
// malformed. num_turns stays advisory accounting except where it authorizes
// the documented zero-turn batch acknowledgement.
type claudeResultCompletion struct {
	index          *int64
	origin         claudeResultOrigin
	terminalReason string
	queuedTurns    *int64
	zeroTurns      bool
}

func decodeClaudeResultCompletion(value providerjson.Value) (claudeResultCompletion, error) {
	fields, err := value.Object("result_index", "origin", "terminal_reason", "queued_turn_count", "num_turns")
	if err != nil {
		return claudeResultCompletion{}, errors.New("result completion metadata is invalid")
	}
	var completion claudeResultCompletion
	if completion.index, err = claudeStrictOptionalCounter(fields, "result_index"); err != nil {
		return claudeResultCompletion{}, err
	}
	if completion.queuedTurns, err = claudeStrictOptionalCounter(fields, "queued_turn_count"); err != nil {
		return claudeResultCompletion{}, err
	}
	if completion.origin, err = decodeClaudeResultOrigin(fields); err != nil {
		return claudeResultCompletion{}, err
	}
	if completion.terminalReason, err = decodeClaudeTerminalReason(fields); err != nil {
		return claudeResultCompletion{}, err
	}
	if turns, turnsErr := claudeStrictOptionalCounter(fields, "num_turns"); turnsErr == nil && turns != nil {
		completion.zeroTurns = *turns == 0
	}
	return completion, nil
}

// refusal returns the completion failure stated by typed metadata. A zero
// queued_turn_count is not proof that no follow-up exists; a positive one is
// queued human input that this single caller turn never submitted.
func (c claudeResultCompletion) refusal() string {
	if c.terminalReason != "" && c.terminalReason != claudeTerminalCompleted {
		return refusalProviderFailed + ": terminal_reason=" + c.terminalReason
	}
	if c.queuedTurns != nil && *c.queuedTurns != 0 {
		return refusalIncomplete
	}
	return ""
}

func claudeStrictOptionalCounter(fields map[string]providerjson.Value, name string) (*int64, error) {
	value, ok := fields[name]
	if !ok {
		return nil, nil
	}
	if claudeCurrentValueIsNull(value) {
		return nil, fmt.Errorf("%s is null", name)
	}
	return claudeCurrentOptionalNonnegativeInt(fields, name)
}

// decodeClaudeResultOrigin reads only the documented discriminators. The
// producer and run identifiers are opaque provider values and stay unselected.
func decodeClaudeResultOrigin(fields map[string]providerjson.Value) (claudeResultOrigin, error) {
	value, ok := fields["origin"]
	if !ok {
		return claudeResultOrigin{}, nil
	}
	originFields, err := value.Object("kind", "subkind")
	if err != nil || claudeCurrentValueIsNull(value) {
		return claudeResultOrigin{}, errors.New("result origin is not an object")
	}
	kind, err := claudeCurrentRequiredText(originFields, "kind", 64)
	if err != nil {
		return claudeResultOrigin{}, errors.New("result origin kind is invalid")
	}
	_, subkindPresent := originFields["subkind"]
	return claudeResultOrigin{present: true, kind: kind, subkindPresent: subkindPresent}, nil
}

func decodeClaudeTerminalReason(fields map[string]providerjson.Value) (string, error) {
	value, ok := fields["terminal_reason"]
	if !ok {
		return "", nil
	}
	reason, err := value.String(128)
	if err != nil || claudeCurrentValueIsNull(value) {
		return "", errors.New("result terminal_reason is not a string")
	}
	if _, failed := claudeFailedTerminalReasons[reason]; reason != claudeTerminalCompleted && !failed {
		return "", errors.New("result terminal_reason is undocumented")
	}
	return reason, nil
}
