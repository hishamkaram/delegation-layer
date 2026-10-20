package opencode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	// acpProtocolVersion is the only ACP major version the adapter negotiates.
	acpProtocolVersion = 1
	// acpMaxFrameBytes keeps every outbound line, plus its newline, within the
	// 32 MiB default incoming-message limit of the ACP SDK ndJsonStream that
	// OpenCode uses. The prompt frame is the only frame that can approach it.
	acpMaxFrameBytes = 32*1024*1024 - 1
	// acpBriefHeader is a visible text block sent before the exact brief.
	// OpenCode joins visible prompt text and runs a native command when the
	// trimmed result starts with "/name"; this header never does, so a brief
	// such as "/compact" is delivered as literal task text. The brief itself
	// is never modified or restricted.
	acpBriefHeader   = "Delegated task brief. The next text block is the exact task supplied by the requester."
	acpClientName    = "delegation-layer"
	acpClientVersion = "1"

	acpMethodInitialize      = "initialize"
	acpMethodSessionNew      = "session/new"
	acpMethodSessionResume   = "session/resume"
	acpMethodSetConfigOption = "session/set_config_option"
	acpMethodSessionPrompt   = "session/prompt"
	acpMethodSessionUpdate   = "session/update"
	acpMethodPermission      = "session/request_permission"
	acpCodeMethodNotFound    = -32601

	acpConfigMode   = "mode"
	acpConfigModel  = "model"
	acpConfigEffort = "effort"

	permissionAllowOnce  = "allow_once"
	permissionRejectOnce = "reject_once"

	// acpWorstRequestID exceeds every request ID the adapter sends. With the
	// longest accepted session ID it bounds the prompt frame before any
	// native request is sent.
	acpWorstRequestID = int64(1) << 62
)

var (
	// errACPFrameTooLarge reports an outbound frame the native reader would
	// refuse. It is raised before the first native request.
	errACPFrameTooLarge = errors.New("encoded OpenCode ACP frame exceeds the native message limit")
	// errACPBrief reports a brief that cannot be carried as ACP text.
	errACPBrief = errors.New("OpenCode ACP text content requires a nonempty UTF-8 brief within the brief size limit")
)

type acpRequestFrame[P any] struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  P      `json:"params"`
}

type acpResultFrame[R any] struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  R               `json:"result"`
}

type acpErrorFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   acpRPCError     `json:"error"`
}

type acpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// acpInitializeParams advertises no client filesystem, terminal,
// elicitation, authentication or child-session capability.
type acpInitializeParams struct {
	ProtocolVersion    int                   `json:"protocolVersion"`
	ClientCapabilities acpClientCapabilities `json:"clientCapabilities"`
	ClientInfo         acpImplementation     `json:"clientInfo"`
}

type acpClientCapabilities struct {
	FS       acpFileSystemCapability `json:"fs"`
	Terminal bool                    `json:"terminal"`
}

type acpFileSystemCapability struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type acpImplementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// acpNoMCPServers always encodes as an empty JSON array: the adapter adds no
// MCP server to the native session.
type acpNoMCPServers struct{}

func (acpNoMCPServers) MarshalJSON() ([]byte, error) { return []byte("[]"), nil }

type acpNewSessionParams struct {
	Cwd        string          `json:"cwd"`
	MCPServers acpNoMCPServers `json:"mcpServers"`
}

type acpResumeSessionParams struct {
	SessionID  string          `json:"sessionId"`
	Cwd        string          `json:"cwd"`
	MCPServers acpNoMCPServers `json:"mcpServers"`
}

type acpSetConfigOptionParams struct {
	SessionID string `json:"sessionId"`
	ConfigID  string `json:"configId"`
	Value     string `json:"value"`
}

type acpPromptParams struct {
	SessionID string          `json:"sessionId"`
	Prompt    json.RawMessage `json:"prompt"`
}

type acpTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type acpPermissionResult struct {
	Outcome acpPermissionOutcome `json:"outcome"`
}

type acpPermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

// encodeACPFrame returns one compact JSON line without its newline. HTML
// escaping is disabled so the size reflects the exact text, and every frame
// is checked against the native limit before the core may write it.
func encodeACPFrame[T any](frame T) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(frame); err != nil {
		return nil, err
	}
	data := bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'})
	if len(data) > acpMaxFrameBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds %d", errACPFrameTooLarge, len(data), acpMaxFrameBytes)
	}
	return data, nil
}

func encodeACPRequest[P any](id int64, method string, params P) ([]byte, error) {
	return encodeACPFrame(acpRequestFrame[P]{JSONRPC: "2.0", ID: id, Method: method, Params: params})
}

// encodeACPPrompt validates the brief and returns the exact ACP prompt array:
// the fixed header text block followed by the unmodified brief. It proves that
// the prompt frame fits the native limit with the longest accepted session
// identifier and request ID, so an unencodable brief fails before initialize.
func encodeACPPrompt(brief []byte) (json.RawMessage, error) {
	if len(brief) == 0 || len(brief) > task.MaxBriefSize || !utf8.Valid(brief) {
		return nil, errACPBrief
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode([]acpTextBlock{{Type: "text", Text: acpBriefHeader}, {Type: "text", Text: string(brief)}}); err != nil {
		return nil, err
	}
	content := json.RawMessage(bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}))
	worst := acpPromptParams{SessionID: strings.Repeat("s", maxSessionIDBytes), Prompt: content}
	if _, err := encodeACPRequest(acpWorstRequestID, acpMethodSessionPrompt, worst); err != nil {
		return nil, err
	}
	return content, nil
}

func encodeACPPermissionReply(id json.RawMessage, outcome acpPermissionOutcome) ([]byte, error) {
	return encodeACPFrame(acpResultFrame[acpPermissionResult]{JSONRPC: "2.0", ID: id, Result: acpPermissionResult{Outcome: outcome}})
}

func encodeACPMethodNotFound(id json.RawMessage) ([]byte, error) {
	return encodeACPFrame(acpErrorFrame{JSONRPC: "2.0", ID: id, Error: acpRPCError{Code: acpCodeMethodNotFound, Message: "method not supported by the Delegate OpenCode ACP client"}})
}
