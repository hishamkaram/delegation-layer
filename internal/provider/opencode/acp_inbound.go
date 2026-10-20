package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	maxACPControlBytes = 4096
	maxACPRequestID    = 64
	maxACPStopReason   = 64
	maxSessionIDBytes  = 128
)

// errACPProtocol reports an ACP frame the adapter cannot accept. It is a
// protocol fault, never a native completion.
var errACPProtocol = errors.New("OpenCode ACP protocol violation")

type acpMessageKind int

const (
	acpResponse acpMessageKind = iota + 1
	acpRequest
	acpNotification
)

// acpMessage holds the selected members of one inbound JSON-RPC 2.0 frame.
// The values are callback-scoped ranges; no frame bytes are retained.
type acpMessage struct {
	kind   acpMessageKind
	method string
	id     providerjson.Value
	params providerjson.Value
	result providerjson.Value
	err    providerjson.Value
	failed bool
}

// decodeACPMessage classifies one frame. Duplicate critical members are
// refused by the record view; unknown members are validated and skipped.
func decodeACPMessage(value providerjson.Value) (acpMessage, error) {
	fields, err := value.Object("jsonrpc", "id", "method", "params", "result", "error")
	if err != nil {
		return acpMessage{}, fmt.Errorf("%w: %w", errACPProtocol, err)
	}
	version, err := acpRequiredString(fields, "jsonrpc", maxACPControlBytes)
	if err != nil || version != "2.0" {
		return acpMessage{}, fmt.Errorf("%w: JSON-RPC version is not 2.0", errACPProtocol)
	}
	id, hasID := fields["id"]
	result, hasResult := fields["result"]
	failure, hasError := fields["error"]
	message := acpMessage{id: id, params: fields["params"], result: result, err: failure, failed: hasError}
	if _, hasMethod := fields["method"]; hasMethod {
		if message.method, err = acpRequiredString(fields, "method", maxACPControlBytes); err != nil || message.method == "" {
			return acpMessage{}, fmt.Errorf("%w: method is not a nonempty string", errACPProtocol)
		}
		if hasResult || hasError {
			return acpMessage{}, fmt.Errorf("%w: request carries a result or error", errACPProtocol)
		}
		message.kind = acpNotification
		if hasID {
			message.kind = acpRequest
		}
		return message, nil
	}
	if !hasID || hasResult == hasError {
		return acpMessage{}, fmt.Errorf("%w: frame is neither a request, notification nor response", errACPProtocol)
	}
	message.kind = acpResponse
	return message, nil
}

// acpResponseID returns the integer identifier of a response. The adapter
// sends only integer IDs, so any other token cannot answer its requests.
func acpResponseID(value providerjson.Value) (int64, error) {
	raw, err := value.Raw(maxACPRequestID)
	if err != nil {
		return 0, fmt.Errorf("%w: response id: %w", errACPProtocol, err)
	}
	id, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || strconv.FormatInt(id, 10) != string(raw) {
		return 0, fmt.Errorf("%w: response id is not an adapter request id", errACPProtocol)
	}
	return id, nil
}

// acpRequestID returns the raw JSON token of a native request identifier so a
// reply can echo it exactly. Only strings and integers are accepted.
func acpRequestID(value providerjson.Value) (json.RawMessage, error) {
	raw, err := value.Raw(maxACPRequestID)
	if err != nil {
		return nil, fmt.Errorf("%w: native request id: %w", errACPProtocol, err)
	}
	if len(raw) > 0 && raw[0] == '"' {
		return raw, nil
	}
	if _, parseErr := strconv.ParseInt(string(raw), 10, 64); parseErr != nil {
		return nil, fmt.Errorf("%w: native request id must be a string or integer", errACPProtocol)
	}
	return raw, nil
}

func acpRequiredString(fields map[string]providerjson.Value, name string, limit int) (string, error) {
	value, ok := fields[name]
	if !ok {
		return "", fmt.Errorf("%w: missing %s", errACPProtocol, name)
	}
	text, err := value.String(limit)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", errACPProtocol, name, err)
	}
	return text, nil
}

func acpRequiredObject(fields map[string]providerjson.Value, name string, names ...string) (map[string]providerjson.Value, error) {
	value, ok := fields[name]
	if !ok {
		return nil, fmt.Errorf("%w: missing %s", errACPProtocol, name)
	}
	selected, err := value.Object(names...)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errACPProtocol, name, err)
	}
	return selected, nil
}

// acpSessionID decodes a native session identifier in OpenCode's ses_ form.
func acpSessionID(fields map[string]providerjson.Value, name string) (string, error) {
	id, err := acpRequiredString(fields, name, maxSessionIDBytes)
	if err != nil || !validSessionID(id) {
		return "", fmt.Errorf("%w: invalid %s", errACPProtocol, name)
	}
	return id, nil
}

// acpErrorCode returns the integer code of a JSON-RPC error object. The native
// message is never copied; it stays in sealed raw stdout.
func acpErrorCode(value providerjson.Value) (int64, error) {
	fields, err := value.Object("code")
	if err != nil {
		return 0, fmt.Errorf("%w: JSON-RPC error: %w", errACPProtocol, err)
	}
	code, ok := fields["code"]
	if !ok {
		return 0, fmt.Errorf("%w: JSON-RPC error has no code", errACPProtocol)
	}
	raw, err := code.Raw(32)
	if err != nil {
		return 0, fmt.Errorf("%w: JSON-RPC error code: %w", errACPProtocol, err)
	}
	parsed, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: JSON-RPC error code is not an integer", errACPProtocol)
	}
	return parsed, nil
}

// acpStopReason decodes a bounded native stop-reason token. It is recorded in
// a refusal, so only lowercase ASCII letters and underscores are accepted.
func acpStopReason(fields map[string]providerjson.Value) (string, error) {
	reason, err := acpRequiredString(fields, "stopReason", maxACPStopReason)
	if err != nil {
		return "", err
	}
	if reason == "" || strings.ContainsFunc(reason, func(r rune) bool { return (r < 'a' || r > 'z') && r != '_' }) {
		return "", fmt.Errorf("%w: stopReason is not a native token", errACPProtocol)
	}
	return reason, nil
}

// acpChildTagged reports whether a session update belongs to a child session.
// Without the child-session capability, OpenCode marks every child update
// with update._meta["opencode/child-session"], an object naming the child.
func acpChildTagged(update map[string]providerjson.Value) (bool, error) {
	meta, ok := update["_meta"]
	if !ok {
		return false, nil
	}
	fields, err := meta.Object("opencode/child-session")
	if err != nil {
		return false, fmt.Errorf("%w: update _meta: %w", errACPProtocol, err)
	}
	if _, child := fields["opencode/child-session"]; !child {
		return false, nil
	}
	child, err := acpRequiredObject(fields, "opencode/child-session", "id")
	if err != nil {
		return false, err
	}
	if id, idErr := acpRequiredString(child, "id", maxSessionIDBytes); idErr != nil || id == "" {
		return false, fmt.Errorf("%w: child-session update has no child id", errACPProtocol)
	}
	return true, nil
}

// acpPermissionOption returns the optionId of the first offered option of the
// wanted kind, or "" when none is offered. Every option must have a nonempty
// optionId and a kind.
func acpPermissionOption(options providerjson.Value, kind string) (string, error) {
	selected := ""
	err := options.Array(func(option providerjson.Value) error {
		fields, err := option.Object("optionId", "kind")
		if err != nil {
			return fmt.Errorf("%w: permission option: %w", errACPProtocol, err)
		}
		id, err := acpRequiredString(fields, "optionId", maxACPControlBytes)
		if err != nil || id == "" {
			return fmt.Errorf("%w: permission option has no optionId", errACPProtocol)
		}
		offered, err := acpRequiredString(fields, "kind", maxACPControlBytes)
		if err != nil {
			return err
		}
		if selected == "" && offered == kind {
			selected = id
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("%w: permission options: %w", errACPProtocol, err)
	}
	return selected, nil
}

// requireTaskSession reports an update or request for a session other than
// the one this task opened.
func requireTaskSession(session, observed string) error {
	if session == "" || observed != session {
		return fmt.Errorf("%w: OpenCode ACP frame names another session", task.ErrIdentityMismatch)
	}
	return nil
}
