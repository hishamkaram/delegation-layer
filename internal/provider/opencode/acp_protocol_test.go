package opencode

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// acpDriver feeds one deterministic native stream to the live protocol the
// way the execution core does, and keeps every frame the protocol queued.
type acpDriver struct {
	t        *testing.T
	protocol *acpProtocol
	recorded []task.SessionIdentity
	frames   []string
	done     bool
}

func newACPDriver(t *testing.T, exchange acpExchange, recordErr error) *acpDriver {
	t.Helper()
	request := profileRequest(exchange.mode)
	request.RequestedConfig = exchange.config()
	expected := task.SessionExpectation{}
	if exchange.expected != "" {
		request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: exchange.expected, PredecessorTaskID: testTaskID}
		expected = task.SessionExpectation{Required: true, ID: exchange.expected}
	}
	driver := &acpDriver{t: t}
	protocol, err := newACPProtocol(request, expected, func(identity task.SessionIdentity) error {
		driver.recorded = append(driver.recorded, identity)
		return recordErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if !protocol.UseRecordCapture() {
		t.Fatal("the ACP protocol must opt into record capture")
	}
	driver.protocol = protocol
	return driver
}

func (d *acpDriver) start(brief string) {
	d.t.Helper()
	frames, err := d.protocol.Start([]byte(brief))
	if err != nil {
		d.t.Fatal(err)
	}
	d.keep(frames)
}

func (d *acpDriver) keep(frames [][]byte) {
	for _, frame := range frames {
		d.frames = append(d.frames, string(frame))
	}
}

// feed delivers native lines until one is refused, and returns that error.
// It enforces the engine contract: no user events, and nothing is sent after
// the protocol reported Done.
func (d *acpDriver) feed(lines ...string) error {
	d.t.Helper()
	for _, line := range lines {
		update, err := d.protocol.Receive([]byte(line))
		if len(update.Events) != 0 {
			d.t.Fatal("machine protocol returned user interaction events")
		}
		if d.done && (len(update.Outbound) != 0 || update.Done) {
			d.t.Fatal("protocol produced output after Done")
		}
		d.keep(update.Outbound)
		d.done = d.done || update.Done
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *acpDriver) mustFeed(lines ...string) {
	d.t.Helper()
	if err := d.feed(lines...); err != nil {
		d.t.Fatal(err)
	}
}

func (d *acpDriver) last() string {
	if len(d.frames) == 0 {
		return ""
	}
	return d.frames[len(d.frames)-1]
}

const acpTestBrief = "Read README.md and reply with its first heading."

func TestACPFreshTaskSendsExactRequestSequence(t *testing.T) {
	exchange := acpExchange{mode: ModeReadOnly, model: "openrouter/elorian/glimpse", effort: "high"}
	driver := newACPDriver(t, exchange, nil)
	driver.start(acpTestBrief)
	lines, promptID := exchange.setup()
	driver.mustFeed(lines...)
	driver.mustFeed(acpThought(acpRootSession), acpRootChunk(acpRootSession, "ACP"), acpRootChunk(acpRootSession, "_NATIVE_RO_OK"), acpPromptResult(promptID, "end_turn"))
	cwd := profileRequest(ModeReadOnly).CanonicalCwd
	want := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false},"clientInfo":{"name":"delegation-layer","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"` + cwd + `","mcpServers":[]}}`,
		`{"jsonrpc":"2.0","id":3,"method":"session/set_config_option","params":{"sessionId":"` + acpRootSession + `","configId":"mode","value":"plan"}}`,
		`{"jsonrpc":"2.0","id":4,"method":"session/set_config_option","params":{"sessionId":"` + acpRootSession + `","configId":"model","value":"openrouter/elorian/glimpse"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"session/set_config_option","params":{"sessionId":"` + acpRootSession + `","configId":"effort","value":"high"}}`,
		`{"jsonrpc":"2.0","id":6,"method":"session/prompt","params":{"sessionId":"` + acpRootSession + `","prompt":[{"type":"text","text":"` + acpBriefHeader + `"},{"type":"text","text":"` + acpTestBrief + `"}]}}`,
	}
	if strings.Join(driver.frames, "\n") != strings.Join(want, "\n") {
		t.Fatalf("frames:\n%s\nwant:\n%s", strings.Join(driver.frames, "\n"), strings.Join(want, "\n"))
	}
	if !driver.done || driver.protocol.Complete() != nil || driver.protocol.Complete() != nil {
		t.Fatalf("end_turn did not complete: done=%t err=%v", driver.done, driver.protocol.Complete())
	}
	if len(driver.recorded) != 1 || driver.recorded[0] != (task.SessionIdentity{Provider: Provider, ConversationID: acpRootSession}) {
		t.Fatalf("recorded identities=%+v, want the exact session once", driver.recorded)
	}
	if !driver.protocol.session.answer.nonblank || driver.protocol.session.outbound.prompt != nil {
		t.Fatal("root answer was not observed or the encoded prompt was retained after Done")
	}
}

func TestACPDefaultEffortAndNoModelSendOnlyTheMode(t *testing.T) {
	exchange := acpExchange{mode: ModeWorkspaceWrite, effort: "default"}
	driver := newACPDriver(t, exchange, nil)
	driver.start(acpTestBrief)
	driver.mustFeed(exchange.stream("end_turn", acpRootChunk(acpRootSession, "DONE"))...)
	configs := 0
	for _, frame := range driver.frames {
		if strings.Contains(frame, `"session/set_config_option"`) {
			configs++
		}
	}
	if configs != 1 || !strings.Contains(driver.frames[2], `"configId":"mode","value":"build"`) || driver.protocol.Complete() != nil {
		t.Fatalf("config frames=%d first=%s complete=%v", configs, driver.frames[2], driver.protocol.Complete())
	}
}

func TestACPContinuationResumesExactSession(t *testing.T) {
	exchange := acpExchange{mode: ModeReadOnly, expected: testSession}
	driver := newACPDriver(t, exchange, nil)
	driver.start(acpTestBrief)
	driver.mustFeed(exchange.stream("end_turn", acpRootChunk(testSession, "token"))...)
	resume := `{"jsonrpc":"2.0","id":2,"method":"session/resume","params":{"sessionId":"` + testSession + `","cwd":"` + profileRequest(ModeReadOnly).CanonicalCwd + `","mcpServers":[]}}`
	if driver.frames[1] != resume || driver.protocol.Complete() != nil {
		t.Fatalf("resume frame=%s complete=%v", driver.frames[1], driver.protocol.Complete())
	}
	if len(driver.recorded) != 1 || driver.recorded[0].ConversationID != testSession {
		t.Fatalf("recorded=%+v want the expected session once", driver.recorded)
	}
	for name, response := range map[string]string{
		"other session": `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"ses_other000000000000000000000","configOptions":[]}}`,
		"invalid id":    `{"jsonrpc":"2.0","id":2,"result":{"sessionId":"latest"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			faulty := newACPDriver(t, exchange, nil)
			faulty.start(acpTestBrief)
			err := faulty.feed(acpInitializeResult(1, true), response)
			if err == nil || len(faulty.recorded) != 0 || faulty.protocol.Complete() == nil {
				t.Fatalf("resume response accepted: err=%v recorded=%d", err, len(faulty.recorded))
			}
		})
	}
	unsupported := newACPDriver(t, exchange, nil)
	unsupported.start(acpTestBrief)
	if err := unsupported.feed(acpInitializeResult(1, false)); err == nil || len(unsupported.frames) != 1 {
		t.Fatalf("missing resume capability was accepted: err=%v frames=%d", err, len(unsupported.frames))
	}
}

func TestACPProtocolRejectsMismatchedExpectations(t *testing.T) {
	request := profileRequest(ModeReadOnly)
	record := func(task.SessionIdentity) error { return nil }
	if _, err := newACPProtocol(request, task.SessionExpectation{Required: true, ID: testSession}, record); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("required expectation without a prior session error=%v", err)
	}
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: testSession, PredecessorTaskID: testTaskID}
	for _, expected := range []task.SessionExpectation{{}, {Required: true, ID: "ses_other"}} {
		if _, err := newACPProtocol(request, expected, record); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("expectation %+v error=%v", expected, err)
		}
	}
	if _, err := newACPProtocol(request, task.SessionExpectation{Required: true, ID: testSession}, nil); err == nil {
		t.Fatal("nil identity recorder accepted")
	}
}

func TestACPConfigurationRequiresEveryAcknowledgementBeforePrompt(t *testing.T) {
	exchange := acpExchange{mode: ModeWorkspaceWrite, model: "anthropic/claude-sonnet", effort: "high"}
	base := []string{acpInitializeResult(1, false), acpNewSessionResult(2, acpRootSession), acpConfigResult(3, "mode=build")}
	cases := map[string]string{
		"missing model":          acpConfigResult(4, "mode=build"),
		"different model":        acpConfigResult(4, "mode=build", "model=anthropic/other"),
		"reverted earlier mode":  acpConfigResult(4, "mode=plan", "model=anthropic/claude-sonnet"),
		"native rejected option": acpErrorResult(4, -32602, "secret native detail"),
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			driver := newACPDriver(t, exchange, nil)
			driver.start(acpTestBrief)
			driver.mustFeed(append(base, response)...)
			err := driver.protocol.Complete()
			if !driver.done || !errors.Is(err, errACPTurnFailed) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("done=%t err=%v, want a refused turn without native text", driver.done, err)
			}
			for _, frame := range driver.frames {
				if strings.Contains(frame, `"session/prompt"`) {
					t.Fatal("prompt was sent without every acknowledgement")
				}
			}
		})
	}
	driver := newACPDriver(t, exchange, nil)
	driver.start(acpTestBrief)
	if err := driver.feed(append(base, acpConfigResult(4, "mode=build", "model=anthropic/claude-sonnet", "model=anthropic/claude-sonnet"))...); !errors.Is(err, errACPProtocol) {
		t.Fatalf("duplicate option acknowledgement error=%v", err)
	}
}

func TestACPStopReasonsOtherThanEndTurnFail(t *testing.T) {
	for _, reason := range []string{"max_tokens", "max_turn_requests", "refusal", "cancelled"} {
		t.Run(reason, func(t *testing.T) {
			exchange := acpExchange{mode: ModeReadOnly}
			driver := newACPDriver(t, exchange, nil)
			driver.start(acpTestBrief)
			driver.mustFeed(exchange.stream(reason, acpRootChunk(acpRootSession, "partial"))...)
			if err := driver.protocol.Complete(); !driver.done || !errors.Is(err, errACPTurnFailed) {
				t.Fatalf("done=%t err=%v", driver.done, err)
			}
		})
	}
	exchange := acpExchange{mode: ModeReadOnly}
	driver := newACPDriver(t, exchange, nil)
	driver.start(acpTestBrief)
	if err := driver.feed(exchange.stream("End Turn")...); !errors.Is(err, errACPProtocol) {
		t.Fatalf("invalid stop reason token error=%v", err)
	}
}

func TestACPIncompleteStreamNeverCompletes(t *testing.T) {
	exchange := acpExchange{mode: ModeReadOnly}
	lines, _ := exchange.setup()
	driver := newACPDriver(t, exchange, nil)
	driver.start(acpTestBrief)
	driver.mustFeed(append(lines, acpRootChunk(acpRootSession, "complete-looking answer"))...)
	if err := driver.protocol.Complete(); driver.done || !errors.Is(err, errACPIncomplete) {
		t.Fatalf("partial EOF done=%t err=%v, want incomplete", driver.done, err)
	}
	unstarted := newACPDriver(t, exchange, nil)
	if err := unstarted.feed(lines[0]); err == nil {
		t.Fatal("native output before Start was accepted")
	}
}

func TestACPIdentityCallbackErrorFaultsBeforeConfiguration(t *testing.T) {
	callbackErr := errors.New("provider.ref write failed")
	exchange := acpExchange{mode: ModeReadOnly}
	driver := newACPDriver(t, exchange, callbackErr)
	driver.start(acpTestBrief)
	err := driver.feed(acpInitializeResult(1, false), acpNewSessionResult(2, acpRootSession))
	if !errors.Is(err, callbackErr) || !errors.Is(driver.protocol.Complete(), callbackErr) || len(driver.frames) != 2 {
		t.Fatalf("callback error=%v complete=%v frames=%d", err, driver.protocol.Complete(), len(driver.frames))
	}
}

func TestACPStartValidatesBriefBeforeInitialize(t *testing.T) {
	for name, brief := range map[string][]byte{"empty": nil, "invalid UTF-8": {0xff, 'a'}} {
		t.Run(name, func(t *testing.T) {
			driver := newACPDriver(t, acpExchange{mode: ModeReadOnly}, nil)
			frames, err := driver.protocol.Start(brief)
			if !errors.Is(err, errACPBrief) || len(frames) != 0 {
				t.Fatalf("frames=%d err=%v", len(frames), err)
			}
		})
	}
	driver := newACPDriver(t, acpExchange{mode: ModeReadOnly}, nil)
	driver.start(acpTestBrief)
	if frames, err := driver.protocol.Start([]byte(acpTestBrief)); err == nil || len(frames) != 0 {
		t.Fatal("second Start was accepted")
	}
}

func TestACPSlashShapedBriefsAreDeliveredAsLiteralTaskText(t *testing.T) {
	for _, brief := range []string{"/compact", "  /init now", "\n/review the diff", "/", "<b>&amp;</b>"} {
		exchange := acpExchange{mode: ModeReadOnly}
		driver := newACPDriver(t, exchange, nil)
		driver.start(brief)
		lines, _ := exchange.setup()
		driver.mustFeed(lines...)
		want := `"prompt":[{"type":"text","text":"` + acpBriefHeader + `"},{"type":"text","text":` + acpQuote(brief) + `}]`
		if !strings.Contains(driver.last(), want) || strings.Contains(driver.last(), `\u003c`) {
			t.Fatalf("brief %q prompt frame does not carry the exact literal text: %s", brief, driver.last())
		}
		if !bytes.HasPrefix([]byte(acpBriefHeader), []byte("Delegated")) {
			t.Fatal("the header must never start with a slash")
		}
	}
}
