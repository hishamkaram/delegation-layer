package opencode

import (
	"errors"
	"fmt"
	"io"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	// acpPredicateVersion is the current ACP output contract: the answer is
	// the final root message run of the prompt.
	acpPredicateVersion = "acp-v2"
	// acpV1PredicateVersion is the original ACP output contract, whose answer
	// is every root text chunk of the prompt. It stays registered for
	// admitted records and their sealed evidence.
	acpV1PredicateVersion = "acp-v1"
)

// acpSelection names how an ACP output contract selects the answer from the
// root agent_message_chunk updates of the prompt.
type acpSelection int

const (
	// acpSelectWholeTurn is acp-v1: every root text chunk is the answer.
	acpSelectWholeTurn acpSelection = iota + 1
	// acpSelectFinalRun is acp-v2: the final root message run is the answer.
	acpSelectFinalRun
)

// acpV1ContractTemplate and acpV2ContractTemplate are the immutable
// descriptions of the ACP result contracts. They describe adapter behavior,
// not an OpenCode release. The mode and its permission policy are
// substituted per mode.
const acpV1ContractTemplate = `{"adapter":"opencode:run","mode":"%s","version":"acp-v1","native":"OpenCode ACP JSON-RPC 2.0 stdio agent launched as opencode acp","permissions":"%s; OpenCode owns configuration, MCP servers, hooks, plugins, skills, authentication, and native policy; the client advertises no filesystem, terminal, elicitation, authentication, or child-session capability and answers every other native request with JSON-RPC -32601","requests":"one request at a time with integer IDs 1, 2, 3...: initialize with protocolVersion 1; session/new for a fresh task or session/resume of the exact recorded session for a continuation; session/set_config_option for mode, then an explicit model, then an explicit non-default effort, each acknowledged by currentValue for it and every earlier option; one session/prompt whose content is a fixed header text block followed by the unmodified brief","events":"every frame is a JSON-RPC 2.0 object; responses must answer the single outstanding request exactly once; every session/update and permission request names the task session; ordered root agent_message_chunk text during the prompt is the answer and updates tagged _meta opencode/child-session are excluded; other notifications and update kinds are advisory; records after the terminal response are validated, and a native request, response, or root answer text after it is a fault","success":"exit_code=0, sealed error empty, a session/prompt response with stopReason end_turn, one matching session identity, and non-whitespace root answer text","failure":"malformed JSON-RPC, a wrong or duplicate response, a session mismatch, a missing resume capability, a JSON-RPC error, an unacknowledged configuration, any other stopReason, missing terminal response, child-only or blank answer, nonzero exit, start failure, or seal error rejects","usage":"the terminal prompt response usage is exposed with unknown scope; absent counters stay null and malformed usage is ignored"}` + "\n"

const acpV2ContractTemplate = `{"adapter":"opencode:run","mode":"%s","version":"acp-v2","native":"OpenCode ACP JSON-RPC 2.0 stdio agent launched as opencode acp","permissions":"%s; OpenCode owns configuration, MCP servers, hooks, plugins, skills, authentication, and native policy; the client advertises no filesystem, terminal, elicitation, authentication, or child-session capability and answers every other native request with JSON-RPC -32601","requests":"one request at a time with integer IDs 1, 2, 3...: initialize with protocolVersion 1; session/new for a fresh task or session/resume of the exact recorded session for a continuation; session/set_config_option for mode, then an explicit model, then an explicit non-default effort, each acknowledged by currentValue for it and every earlier option; one session/prompt whose content is a fixed header text block followed by the unmodified brief","events":"every frame is a JSON-RPC 2.0 object; responses must answer the single outstanding request exactly once; every session/update and permission request names the task session; root agent_message_chunk updates during the prompt form message runs, each a maximal sequence of root chunks naming one messageId compared by its decoded string, and updates tagged _meta opencode/child-session are excluded; thoughts, tool calls, plans, usage, child updates, other update kinds, and other notifications are advisory and never end a run; the answer is the ordered root text of the final run, whose nontext chunks count toward the run; when a root chunk has no messageId or a null one, or the final messageId also names an earlier run, the answer is all ordered root text of the prompt and a diagnostic reports the unavailable or ambiguous boundary; records after the terminal response are validated, and a native request, response, or root answer text after it is a fault","success":"exit_code=0, sealed error empty, a session/prompt response with stopReason end_turn, one matching session identity, and non-whitespace selected answer text","failure":"malformed JSON-RPC, a duplicated root messageId or one that is neither a string nor null, a wrong or duplicate response, a session mismatch, a missing resume capability, a JSON-RPC error, an unacknowledged configuration, any other stopReason, missing terminal response, child-only or blank selected answer, nonzero exit, start failure, or seal error rejects","usage":"the terminal prompt response usage is exposed with unknown scope; absent counters stay null and malformed usage is ignored"}` + "\n"

func acpContractForMode(mode string) string { return acpContract(acpSelectFinalRun, mode) }

func acpContract(selection acpSelection, mode string) string {
	template := acpV2ContractTemplate
	if selection == acpSelectWholeTurn {
		template = acpV1ContractTemplate
	}
	switch mode {
	case ModeReadOnly:
		return fmt.Sprintf(template, mode, "read-only selects the native plan agent through session/set_config_option and answers native permission requests with the offered reject_once option, or cancelled when none is offered")
	case ModeWorkspaceWrite:
		return fmt.Sprintf(template, mode, "workspace-write selects the native build agent through session/set_config_option and answers native permission requests with the offered allow_once option; the adapter does not impose an independent workspace boundary")
	default:
		return ""
	}
}

// acpReferenceForMode returns the current (acp-v2) reference for mode.
func acpReferenceForMode(mode string) task.PredicateRef { return acpReference(acpSelectFinalRun, mode) }

// acpV1ReferenceForMode returns the original acp-v1 reference for mode.
func acpV1ReferenceForMode(mode string) task.PredicateRef {
	return acpReference(acpSelectWholeTurn, mode)
}

func acpReference(selection acpSelection, mode string) task.PredicateRef {
	contract := acpContract(selection, mode)
	if contract == "" {
		return task.PredicateRef{}
	}
	version := acpPredicateVersion
	if selection == acpSelectWholeTurn {
		version = acpV1PredicateVersion
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: version, SHA256: task.ComputeSHA256([]byte(contract))}
}

// isKnownACPReference reports whether reference is exactly one registered
// ACP output contract for mode.
func isKnownACPReference(mode string, reference task.PredicateRef) bool {
	if reference == (task.PredicateRef{}) {
		return false
	}
	return reference.Equal(acpReferenceForMode(mode)) || reference.Equal(acpV1ReferenceForMode(mode))
}

// acpInterpreter replays the sealed ACP stdout through the same reducer the
// live protocol used. Collection and recheck are offline: it starts no
// protocol factory and no native process. Its selection is fixed by the
// recorded predicate version.
type acpInterpreter struct {
	mode      string
	selection acpSelection
}

func newACPV1Interpreter(mode string) predicate.Interpreter {
	return acpInterpreter{mode: mode, selection: acpSelectWholeTurn}
}

func (v acpInterpreter) Reference() task.PredicateRef { return acpReference(v.selection, v.mode) }

func (v acpInterpreter) Evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if err := validateEvaluationInput(input, raw, out, v.Reference()); err != nil {
		return task.Interpretation{}, err
	}
	if input.RequestedConfig.Permission != v.mode {
		return task.Interpretation{}, fmt.Errorf("%w: OpenCode ACP request context does not match the predicate mode", task.ErrIdentityMismatch)
	}
	if v.selection == acpSelectFinalRun {
		return evaluateFinalRun(input, raw, out)
	}
	return evaluateWholeTurn(input, raw, out)
}

// evaluateWholeTurn is the acp-v1 interpretation: every root text chunk of
// the prompt is the answer.
func evaluateWholeTurn(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	session, err := replayACP(input, raw, nil, nil)
	if err != nil {
		return task.Interpretation{}, err
	}
	if refusal := acpRefusal(input, session); refusal != "" {
		return acpRejection(refusal, session), nil
	}
	streamed, err := replayACP(input, raw, out, nil)
	if err != nil {
		return task.Interpretation{}, err
	}
	if streamed.completion() != nil || streamed.session != session.session {
		return task.Interpretation{}, fmt.Errorf("%w: OpenCode ACP replay was not deterministic", task.ErrEvidenceFault)
	}
	return acpCommitted(session), nil
}

// acpRejection rejects with the refusal diagnostic, then any answer
// selection diagnostics, then the optional usage warning.
func acpRejection(refusal string, session *acpSession, selection ...task.OutputDiagnostic) task.Interpretation {
	result := rejectedWithUsage(refusal, session.usage.records(task.UsageReliabilityUnreliable))
	diagnostics := append([]task.OutputDiagnostic{acpDiagnostic(refusal)}, selection...)
	result.Diagnostics = append(diagnostics, acpUsageWarning(session.usage)...)
	return result
}

// acpCommitted commits the replayed session with any answer selection
// diagnostics, then the optional usage warning.
func acpCommitted(session *acpSession, selection ...task.OutputDiagnostic) task.Interpretation {
	diagnostics := append(append([]task.OutputDiagnostic(nil), selection...), acpUsageWarning(session.usage)...)
	return task.Interpretation{
		Verdict:     task.VerdictCommitted,
		Session:     &task.SessionIdentity{Provider: Provider, ConversationID: session.session},
		Usage:       session.usage.records(task.UsageReliabilityReported),
		Diagnostics: diagnostics,
	}
}

// replayACP feeds every sealed stdout record to a fresh reducer. Semantic
// faults stay in the reducer; only evidence read faults, answer sink
// failures, and messageId read failures are returned as errors. runs, when
// present, observes root message runs and selects the streamed text.
func replayACP(input predicate.Input, raw predicate.Evidence, sink io.Writer, runs *acpMessageRuns) (*acpSession, error) {
	session, err := newACPSession(input.RequestedConfig.Permission, input.RequestedConfig, input.ExpectedSession, nil)
	if err != nil {
		return nil, err
	}
	if _, err = session.begin(); err != nil {
		return nil, err
	}
	session.answer.sink = sink
	session.answer.runs = runs
	stdoutErr := raw.Read(predicate.Stdout, func(reader io.Reader) error {
		semanticErr, readErr := providerjson.ReadRecords(reader, func(value providerjson.Value, _ uint64) error {
			_, receiveErr := session.receive(value)
			return receiveErr
		})
		// A semantic fault is evidence, kept by the reducer for the verdict.
		if semanticErr != nil && session.fault == nil {
			session.fault = semanticErr
		}
		return readErr
	})
	stderrErr := raw.Read(predicate.Stderr, commonprovider.DrainReader)
	if err = errors.Join(stdoutErr, stderrErr, session.answer.sinkErr, runs.readFault()); err != nil {
		return nil, err
	}
	return session, nil
}

// acpRefusal is the acp-v1 refusal: the turn refusal, then a blank answer
// over every root text chunk.
func acpRefusal(input predicate.Input, session *acpSession) string {
	if refusal := acpTurnRefusal(input, session); refusal != "" {
		return refusal
	}
	if !session.answer.seen || !session.answer.nonblank {
		return refusalEmpty
	}
	return ""
}

// acpTurnRefusal rejects a sealed turn that failed, did not complete, or
// names another session, independent of how the answer is selected.
func acpTurnRefusal(input predicate.Input, session *acpSession) string {
	if refusal := sealRefusal(input.Seal); refusal != "" {
		return refusal
	}
	if err := session.completion(); err != nil {
		switch {
		case errors.Is(err, task.ErrIdentityMismatch):
			return refusalIdentity
		case errors.Is(err, errACPIncomplete):
			return refusalIncomplete
		case errors.Is(err, errACPTurnFailed):
			return refusalProviderFailed + ": " + acpTerminalCode(session)
		default:
			return refusalMalformed
		}
	}
	if !identityMatches(input, session.session) {
		return refusalIdentity
	}
	return ""
}

// acpTerminalCode is a bounded refusal suffix. Only a validated stop-reason
// token is copied; native error messages never enter the refusal.
func acpTerminalCode(session *acpSession) string {
	switch session.terminal {
	case acpTerminalNativeError:
		return "native_error"
	case acpTerminalUnacknowledged:
		return "configuration_unacknowledged"
	case acpTerminalStopped:
		return "stop_reason=" + session.stopReason
	case acpTerminalNone, acpTerminalEndTurn:
		return "incomplete"
	default:
		return "unknown"
	}
}

func acpDiagnostic(refusal string) task.OutputDiagnostic {
	diagnostic := openCodeCurrentDiagnostic(refusal)
	if refusal == refusalIncomplete {
		diagnostic.Message = "OpenCode ACP output did not contain the terminal response to the task prompt."
	}
	return diagnostic
}

func acpUsageWarning(usage acpUsage) []task.OutputDiagnostic {
	if !usage.present || !usage.invalid {
		return nil
	}
	return []task.OutputDiagnostic{{
		Code:    "optional_usage_unavailable",
		Message: "Optional OpenCode usage could not be interpreted; the answer was assessed independently.",
		Field:   "usage",
	}}
}
