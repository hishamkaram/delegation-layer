package opencode

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// promptOpen returns a driver whose prompt is outstanding, and the prompt ID.
func promptOpen(t *testing.T, exchange acpExchange) (*acpDriver, int) {
	t.Helper()
	driver := newACPDriver(t, exchange, nil)
	driver.start(acpTestBrief)
	lines, promptID := exchange.setup()
	driver.mustFeed(lines...)
	if !strings.Contains(driver.last(), `"session/prompt"`) {
		t.Fatal("prompt was not sent")
	}
	return driver, promptID
}

func TestACPPermissionPolicyFollowsTheRequestedMode(t *testing.T) {
	cases := []struct {
		name, mode, id string
		kinds          []string
		want           string
	}{
		{"write allows once", ModeWorkspaceWrite, "0", []string{"allow_always", "allow_once", "reject_once"}, `{"jsonrpc":"2.0","id":0,"result":{"outcome":{"outcome":"selected","optionId":"opt-1"}}}`},
		{"read-only rejects once", ModeReadOnly, `"perm-7"`, []string{"allow_once", "allow_always", "reject_once"}, `{"jsonrpc":"2.0","id":"perm-7","result":{"outcome":{"outcome":"selected","optionId":"opt-2"}}}`},
		{"read-only without reject cancels", ModeReadOnly, "12", []string{"allow_once"}, `{"jsonrpc":"2.0","id":12,"result":{"outcome":{"outcome":"cancelled"}}}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			exchange := acpExchange{mode: testCase.mode}
			driver, promptID := promptOpen(t, exchange)
			driver.mustFeed(acpPermissionRequest(testCase.id, acpRootSession, testCase.kinds...))
			if driver.last() != testCase.want {
				t.Fatalf("reply=%s want %s", driver.last(), testCase.want)
			}
			driver.mustFeed(acpRootChunk(acpRootSession, "DONE"), acpPromptResult(promptID, "end_turn"))
			if driver.protocol.Complete() != nil {
				t.Fatal(driver.protocol.Complete())
			}
		})
	}
}

func TestACPPermissionRequestsAreValidated(t *testing.T) {
	malformed := `{"jsonrpc":"2.0","id":1,"method":"session/request_permission","params":{"sessionId":"` + acpRootSession + `","toolCall":{},"options":[{"kind":"allow_once"}]}}`
	cases := map[string]struct {
		mode, line string
		want       error
	}{
		"write without allow_once": {ModeWorkspaceWrite, acpPermissionRequest("1", acpRootSession, "reject_once"), errACPProtocol},
		"other session":            {ModeWorkspaceWrite, acpPermissionRequest("1", "ses_other000000000000000000000", "allow_once"), task.ErrIdentityMismatch},
		"option without id":        {ModeWorkspaceWrite, malformed, errACPProtocol},
		"no tool call":             {ModeReadOnly, strings.Replace(acpPermissionRequest("1", acpRootSession, "reject_once"), `"toolCall":{"toolCallId":"call_1","title":"edit","kind":"edit","status":"pending"},`, "", 1), errACPProtocol},
		"fractional id":            {ModeReadOnly, acpPermissionRequest("1.5", acpRootSession, "reject_once"), errACPProtocol},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			driver, _ := promptOpen(t, acpExchange{mode: testCase.mode})
			frames := len(driver.frames)
			if err := driver.feed(testCase.line); !errors.Is(err, testCase.want) || len(driver.frames) != frames {
				t.Fatalf("error=%v want %v; replies=%d", err, testCase.want, len(driver.frames)-frames)
			}
		})
	}
	early := newACPDriver(t, acpExchange{mode: ModeWorkspaceWrite}, nil)
	early.start(acpTestBrief)
	if err := early.feed(acpInitializeResult(1, false), acpNewSessionResult(2, acpRootSession), acpPermissionRequest("0", acpRootSession, "allow_once")); !errors.Is(err, errACPProtocol) {
		t.Fatalf("permission before the prompt error=%v", err)
	}
}

func TestACPUnknownRequestsGetMethodNotFoundAndNotificationsAreAdvisory(t *testing.T) {
	exchange := acpExchange{mode: ModeWorkspaceWrite}
	driver, promptID := promptOpen(t, exchange)
	driver.mustFeed(`{"jsonrpc":"2.0","id":"fs-1","method":"fs/read_text_file","params":{"path":"/etc/passwd"}}`)
	want := `{"jsonrpc":"2.0","id":"fs-1","error":{"code":-32601,"message":"method not supported by the Delegate OpenCode ACP client"}}`
	if driver.last() != want {
		t.Fatalf("reply=%s want %s", driver.last(), want)
	}
	frames := len(driver.frames)
	driver.mustFeed(
		`{"jsonrpc":"2.0","method":"opencode/unknown_notice","params":{"anything":[1,2,3]}}`,
		`{"jsonrpc":"2.0","method":"$/progress"}`,
		acpUpdate(acpRootSession, `{"sessionUpdate":"plan","entries":[]}`),
		acpUpdate(acpRootSession, `{"sessionUpdate":"brand_new_kind","payload":{"x":1}}`),
		acpUpdate(acpRootSession, `{"sessionUpdate":"agent_message_chunk","content":{"type":"image","data":"AAAA","mimeType":"image/png"}}`),
		acpRootChunk(acpRootSession, "answer"),
		acpPromptResult(promptID, "end_turn"),
	)
	if len(driver.frames) != frames || driver.protocol.Complete() != nil {
		t.Fatalf("advisory notifications produced %d frames, complete=%v", len(driver.frames)-frames, driver.protocol.Complete())
	}
}

func TestACPMalformedAndMismatchedFramesFault(t *testing.T) {
	cases := map[string]string{
		"not JSON":                  `{"jsonrpc":"2.0",`,
		"blank":                     ``,
		"wrong version":             `{"jsonrpc":"1.0","method":"session/update","params":{}}`,
		"missing version":           `{"method":"session/update","params":{}}`,
		"result and error":          `{"jsonrpc":"2.0","id":5,"result":{},"error":{"code":1}}`,
		"response without id":       `{"jsonrpc":"2.0","result":{}}`,
		"duplicate id member":       `{"jsonrpc":"2.0","id":5,"id":5,"result":{"stopReason":"end_turn"}}`,
		"wrong response id":         acpPromptResult(9, "end_turn"),
		"string response id":        `{"jsonrpc":"2.0","id":"4","result":{"stopReason":"end_turn"}}`,
		"repeated config response":  acpConfigResult(3, "mode=plan"),
		"update for another":        acpRootChunk("ses_other000000000000000000000", "x"),
		"update without session":    `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"plan"}}}`,
		"non-object meta":           acpUpdate(acpRootSession, `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"},"_meta":[]}`),
		"child tag without id":      acpUpdate(acpRootSession, `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"},"_meta":{"opencode/child-session":{}}}`),
		"text that is not a string": acpUpdate(acpRootSession, `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":7}}`),
		"control in text":           acpUpdate(acpRootSession, "{\"sessionUpdate\":\"agent_message_chunk\",\"content\":{\"type\":\"text\",\"text\":\"a\x01b\"}}"),
		"error without code":        `{"jsonrpc":"2.0","id":4,"error":{"message":"x"}}`,
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			driver, _ := promptOpen(t, acpExchange{mode: ModeReadOnly})
			err := driver.feed(line)
			if err == nil || driver.done || driver.protocol.Complete() == nil {
				t.Fatalf("frame accepted: err=%v done=%t", err, driver.done)
			}
			if later := driver.feed(acpPromptResult(4, "end_turn")); later == nil || driver.protocol.Complete() == nil {
				t.Fatal("a later terminal response erased the fault")
			}
		})
	}
}

func TestACPRootTextOutsideThePromptFaults(t *testing.T) {
	driver := newACPDriver(t, acpExchange{mode: ModeReadOnly}, nil)
	driver.start(acpTestBrief)
	if err := driver.feed(acpInitializeResult(1, false), acpNewSessionResult(2, acpRootSession), acpRootChunk(acpRootSession, "early")); !errors.Is(err, errACPProtocol) {
		t.Fatalf("root text before the prompt error=%v", err)
	}
	child := newACPDriver(t, acpExchange{mode: ModeReadOnly}, nil)
	child.start(acpTestBrief)
	if err := child.feed(acpInitializeResult(1, false), acpNewSessionResult(2, acpRootSession), acpChildChunk(acpRootSession, "child")); err != nil {
		t.Fatalf("child-tagged text is not root text: %v", err)
	}
}

func TestACPRecordsAfterDoneAreValidatedAndCannotReply(t *testing.T) {
	cases := map[string]string{
		"native request":     acpPermissionRequest("3", acpRootSession, "reject_once"),
		"unknown request":    `{"jsonrpc":"2.0","id":9,"method":"terminal/create","params":{}}`,
		"duplicate terminal": acpPromptResult(4, "end_turn"),
		"trailing root text": acpRootChunk(acpRootSession, "late"),
		"malformed trailer":  `not json`,
		"another session":    acpUpdate("ses_other000000000000000000000", `{"sessionUpdate":"usage_update","used":1,"size":2}`),
		"unknown response":   `{"jsonrpc":"2.0","id":99,"result":{}}`,
	}
	for name, trailer := range cases {
		t.Run(name, func(t *testing.T) {
			exchange := acpExchange{mode: ModeReadOnly}
			driver := newACPDriver(t, exchange, nil)
			driver.start(acpTestBrief)
			driver.mustFeed(exchange.stream("end_turn", acpRootChunk(acpRootSession, "answer"))...)
			if driver.protocol.Complete() != nil {
				t.Fatal("setup did not complete")
			}
			frames := len(driver.frames)
			if err := driver.feed(trailer); err == nil || driver.protocol.Complete() == nil || len(driver.frames) != frames {
				t.Fatalf("trailer accepted after Done: err=%v replies=%d", err, len(driver.frames)-frames)
			}
		})
	}
	exchange := acpExchange{mode: ModeReadOnly}
	driver := newACPDriver(t, exchange, nil)
	driver.start(acpTestBrief)
	driver.mustFeed(exchange.stream("end_turn", acpRootChunk(acpRootSession, "answer"))...)
	driver.mustFeed(acpUpdate(acpRootSession, `{"sessionUpdate":"usage_update","used":1,"size":2}`), acpChildChunk(acpRootSession, "late child"), `{"jsonrpc":"2.0","method":"opencode/notice"}`)
	if driver.protocol.Complete() != nil {
		t.Fatalf("advisory records after Done changed the verdict: %v", driver.protocol.Complete())
	}
}

func TestACPPromptFrameIsBoundedBeforeInitialize(t *testing.T) {
	overhead := acpPromptOverhead(t)
	room := acpMaxFrameBytes - overhead
	// Escaped control bytes expand six times, so a brief within the 8 MiB
	// brief limit can reach the 32 MiB native frame limit.
	controls := room / 6
	exact := strings.Repeat("\x01", controls) + strings.Repeat("a", room-controls*6)
	if len(exact) > task.MaxBriefSize {
		t.Fatalf("boundary brief %d bytes exceeds the brief limit", len(exact))
	}
	driver := newACPDriver(t, acpExchange{mode: ModeReadOnly}, nil)
	frames, err := driver.protocol.Start([]byte(exact))
	if err != nil || len(frames) != 1 {
		t.Fatalf("a prompt frame of exactly %d bytes was refused: %v", acpMaxFrameBytes, err)
	}
	over := newACPDriver(t, acpExchange{mode: ModeReadOnly}, nil)
	frames, err = over.protocol.Start([]byte(exact + "a"))
	if !errors.Is(err, errACPFrameTooLarge) || len(frames) != 0 {
		t.Fatalf("one byte over the limit: frames=%d err=%v", len(frames), err)
	}
	if _, err = encodeACPPrompt(make([]byte, task.MaxBriefSize+1)); !errors.Is(err, errACPBrief) {
		t.Fatalf("brief above the brief limit error=%v", err)
	}
}

// acpPromptOverhead is the size of the worst-case prompt frame for an empty
// brief: the longest session ID and largest request ID the bound assumes.
func acpPromptOverhead(t *testing.T) int {
	t.Helper()
	frame, err := encodeACPRequest(acpWorstRequestID, acpMethodSessionPrompt, acpPromptParams{
		SessionID: strings.Repeat("s", maxSessionIDBytes),
		Prompt:    []byte(fmt.Sprintf(`[{"type":"text","text":%s},{"type":"text","text":""}]`, acpQuote(acpBriefHeader))),
	})
	if err != nil {
		t.Fatal(err)
	}
	return len(frame)
}

func TestACPStreamsLargeIrrelevantMetadata(t *testing.T) {
	exchange := acpExchange{mode: ModeWorkspaceWrite}
	driver, promptID := promptOpen(t, exchange)
	large := strings.Repeat("x", 3*1024*1024)
	tool := acpUpdate(acpRootSession, `{"sessionUpdate":"tool_call_update","toolCallId":"call_1","status":"completed","rawInput":{"content":`+acpQuote(large)+`},"rawOutput":{"output":`+acpQuote(large)+`}}`)
	driver.mustFeed(tool, acpRootChunk(acpRootSession, "DONE"), acpPromptResult(promptID, "end_turn"))
	if driver.protocol.Complete() != nil {
		t.Fatalf("large advisory metadata changed the verdict: %v", driver.protocol.Complete())
	}
}
