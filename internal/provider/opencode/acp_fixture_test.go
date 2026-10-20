package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// These are deterministic ACP frames shaped like the OpenCode 2.0.32 native
// evidence (initialize capabilities, configOptions, session/update kinds,
// child-session tags, permission options, prompt usage). They are unit
// fixtures, not native acceptance: only Root's native runs prove behavior of
// an installed OpenCode.

const acpRootSession = "ses_7e2a1c9d4b3f8e6a5c1d2b3f4e5a"

// acpQuote encodes text as the adapter does: a JSON string without HTML
// escaping.
func acpQuote(text string) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(text); err != nil {
		panic(err)
	}
	return strings.TrimSuffix(buffer.String(), "\n")
}

func acpInitializeResult(id int, resume bool) string {
	sessionCapabilities := `{"close":{},"fork":{},"list":{}}`
	if resume {
		sessionCapabilities = `{"close":{},"fork":{},"list":{},"resume":{}}`
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"promptCapabilities":{"image":true},"sessionCapabilities":%s,"_meta":{"opencode/child-session-updates":true}},"authMethods":[{"id":"opencode-login","name":"Login","description":"fixture"}],"agentInfo":{"name":"OpenCode","version":"2.0.32"}}}`, id, sessionCapabilities)
}

// acpConfigOptions renders configOptions with the given id=value pairs.
func acpConfigOptions(pairs ...string) string {
	var options []string
	for _, pair := range pairs {
		id, value, _ := strings.Cut(pair, "=")
		options = append(options, fmt.Sprintf(`{"id":%s,"name":"fixture","category":%s,"type":"select","currentValue":%s,"options":[{"value":%s,"name":"fixture"}]}`, acpQuote(id), acpQuote(id), acpQuote(value), acpQuote(value)))
	}
	return "[" + strings.Join(options, ",") + "]"
}

func acpNewSessionResult(id int, session string, pairs ...string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"sessionId":%s,"configOptions":%s}}`, id, acpQuote(session), acpConfigOptions(pairs...))
}

func acpResumeResult(id int, pairs ...string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"configOptions":%s}}`, id, acpConfigOptions(pairs...))
}

func acpConfigResult(id int, pairs ...string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"configOptions":%s}}`, id, acpConfigOptions(pairs...))
}

func acpPromptResult(id int, stopReason string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"stopReason":%s,"usage":{"inputTokens":13083,"outputTokens":24,"totalTokens":28758,"thoughtTokens":35,"cachedReadTokens":15616},"_meta":{}}}`, id, acpQuote(stopReason))
}

func acpErrorResult(id int, code int, message string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":%d,"message":%s}}`, id, code, acpQuote(message))
}

func acpUpdate(session, body string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%s,"update":%s}}`, acpQuote(session), body)
}

func acpRootChunk(session, text string) string {
	return acpUpdate(session, fmt.Sprintf(`{"sessionUpdate":"agent_message_chunk","messageId":"msg_1530bf85f001Z7Zy1d4PADAbZD","content":{"type":"text","text":%s}}`, acpQuote(text)))
}

func acpChildChunk(session, text string) string {
	return acpUpdate(session, fmt.Sprintf(`{"sessionUpdate":"agent_message_chunk","messageId":"msg_child","content":{"type":"text","text":%s},"_meta":{"opencode/child-session":{"id":"ses_child00000000000000000000a","parentID":%s,"depth":1,"title":"fixture"}}}`, acpQuote(text), acpQuote(session)))
}

func acpThought(session string) string {
	return acpUpdate(session, `{"sessionUpdate":"agent_thought_chunk","messageId":"msg_t","content":{"type":"text","text":"thinking"}}`)
}

func acpPermissionRequest(id, session string, kinds ...string) string {
	var options []string
	for index, kind := range kinds {
		options = append(options, fmt.Sprintf(`{"optionId":"opt-%d","kind":%s,"name":%s}`, index, acpQuote(kind), acpQuote(kind)))
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"session/request_permission","params":{"sessionId":%s,"toolCall":{"toolCallId":"call_1","title":"edit","kind":"edit","status":"pending"},"options":[%s]}}`, id, acpQuote(session), strings.Join(options, ","))
}

// acpExchange is one deterministic native stream with the adapter frames it
// must produce: setup for the given expectation and settings, then the given
// prompt-phase records, then the terminal response.
type acpExchange struct {
	mode     string
	model    string
	effort   string
	expected string
}

func (e acpExchange) config() task.TaskConfig {
	return task.TaskConfig{Permission: e.mode, Model: e.model, Effort: e.effort}
}

// setup returns the native responses through the last configuration ACK and
// the ID the prompt request will carry.
func (e acpExchange) setup() ([]string, int) {
	agent := nativeReadOnlyAgent
	if e.mode == ModeWorkspaceWrite {
		agent = nativeWorkspaceWriteAgent
	}
	acked := []string{"mode=" + agent}
	if e.model != "" {
		acked = append(acked, "model="+e.model)
	}
	if explicitEffort(e.effort) {
		acked = append(acked, "effort="+e.effort)
	}
	lines := []string{acpInitializeResult(1, e.expected != "")}
	if e.expected == "" {
		lines = append(lines, acpNewSessionResult(2, acpRootSession, "mode=build", "model=opencode/big-pickle"))
	} else {
		lines = append(lines, acpResumeResult(2, "mode=build"))
	}
	lines = append(lines, acpUpdate(e.session(), `{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"init","description":"fixture"}]}`))
	for index := range acked {
		reported := append([]string(nil), acked[:index+1]...)
		if e.model == "" {
			// OpenCode reports every option; an unrequested one is advisory.
			reported = append(reported, "model=opencode/big-pickle")
		}
		lines = append(lines, acpConfigResult(3+index, reported...))
	}
	return lines, 3 + len(acked)
}

func (e acpExchange) session() string {
	if e.expected != "" {
		return e.expected
	}
	return acpRootSession
}

// stream returns a complete native stream: setup, body, and the terminal
// prompt response with stopReason.
func (e acpExchange) stream(stopReason string, body ...string) []string {
	lines, promptID := e.setup()
	lines = append(lines, body...)
	return append(lines, acpPromptResult(promptID, stopReason))
}

func acpStdout(lines []string) []byte {
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}
