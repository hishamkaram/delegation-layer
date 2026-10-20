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

const acpPredicateVersion = "acp-v1"

// acpContractTemplate is the immutable description of the ACP result
// contract. It describes adapter behavior, not an OpenCode release. The mode
// and its permission policy are substituted per mode.
const acpContractTemplate = `{"adapter":"opencode:run","mode":"%s","version":"acp-v1","native":"OpenCode ACP JSON-RPC 2.0 stdio agent launched as opencode acp","permissions":"%s; OpenCode owns configuration, MCP servers, hooks, plugins, skills, authentication, and native policy; the client advertises no filesystem, terminal, elicitation, authentication, or child-session capability and answers every other native request with JSON-RPC -32601","requests":"one request at a time with integer IDs 1, 2, 3...: initialize with protocolVersion 1; session/new for a fresh task or session/resume of the exact recorded session for a continuation; session/set_config_option for mode, then an explicit model, then an explicit non-default effort, each acknowledged by currentValue for it and every earlier option; one session/prompt whose content is a fixed header text block followed by the unmodified brief","events":"every frame is a JSON-RPC 2.0 object; responses must answer the single outstanding request exactly once; every session/update and permission request names the task session; ordered root agent_message_chunk text during the prompt is the answer and updates tagged _meta opencode/child-session are excluded; other notifications and update kinds are advisory; records after the terminal response are validated, and a native request, response, or root answer text after it is a fault","success":"exit_code=0, sealed error empty, a session/prompt response with stopReason end_turn, one matching session identity, and non-whitespace root answer text","failure":"malformed JSON-RPC, a wrong or duplicate response, a session mismatch, a missing resume capability, a JSON-RPC error, an unacknowledged configuration, any other stopReason, missing terminal response, child-only or blank answer, nonzero exit, start failure, or seal error rejects","usage":"the terminal prompt response usage is exposed with unknown scope; absent counters stay null and malformed usage is ignored"}` + "\n"

func acpContractForMode(mode string) string {
	switch mode {
	case ModeReadOnly:
		return fmt.Sprintf(acpContractTemplate, mode, "read-only selects the native plan agent through session/set_config_option and answers native permission requests with the offered reject_once option, or cancelled when none is offered")
	case ModeWorkspaceWrite:
		return fmt.Sprintf(acpContractTemplate, mode, "workspace-write selects the native build agent through session/set_config_option and answers native permission requests with the offered allow_once option; the adapter does not impose an independent workspace boundary")
	default:
		return ""
	}
}

func acpReferenceForMode(mode string) task.PredicateRef {
	contract := acpContractForMode(mode)
	if contract == "" {
		return task.PredicateRef{}
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: acpPredicateVersion, SHA256: task.ComputeSHA256([]byte(contract))}
}

// acpInterpreter replays the sealed ACP stdout through the same reducer the
// live protocol used. Collection and recheck are offline: it starts no
// protocol factory and no native process.
type acpInterpreter struct{ mode string }

func (v acpInterpreter) Reference() task.PredicateRef { return acpReferenceForMode(v.mode) }

func (v acpInterpreter) Evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if err := validateEvaluationInput(input, raw, out, v.Reference()); err != nil {
		return task.Interpretation{}, err
	}
	if input.RequestedConfig.Permission != v.mode {
		return task.Interpretation{}, fmt.Errorf("%w: OpenCode ACP request context does not match the predicate mode", task.ErrIdentityMismatch)
	}
	session, err := replayACP(input, raw, nil)
	if err != nil {
		return task.Interpretation{}, err
	}
	if refusal := acpRefusal(input, session); refusal != "" {
		result := rejectedWithUsage(refusal, session.usage.records(task.UsageReliabilityUnreliable))
		result.Diagnostics = append([]task.OutputDiagnostic{acpDiagnostic(refusal)}, acpUsageWarning(session.usage)...)
		return result, nil
	}
	streamed, err := replayACP(input, raw, out)
	if err != nil {
		return task.Interpretation{}, err
	}
	if streamed.completion() != nil || streamed.session != session.session {
		return task.Interpretation{}, fmt.Errorf("%w: OpenCode ACP replay was not deterministic", task.ErrEvidenceFault)
	}
	return task.Interpretation{
		Verdict:     task.VerdictCommitted,
		Session:     &task.SessionIdentity{Provider: Provider, ConversationID: session.session},
		Usage:       session.usage.records(task.UsageReliabilityReported),
		Diagnostics: acpUsageWarning(session.usage),
	}, nil
}

// replayACP feeds every sealed stdout record to a fresh reducer. Semantic
// faults stay in the reducer; only evidence read faults and answer sink
// failures are returned as errors.
func replayACP(input predicate.Input, raw predicate.Evidence, sink io.Writer) (*acpSession, error) {
	session, err := newACPSession(input.RequestedConfig.Permission, input.RequestedConfig, input.ExpectedSession, nil)
	if err != nil {
		return nil, err
	}
	if _, err = session.begin(); err != nil {
		return nil, err
	}
	session.answer.sink = sink
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
	if err = errors.Join(stdoutErr, stderrErr, session.answer.sinkErr); err != nil {
		return nil, err
	}
	return session, nil
}

func acpRefusal(input predicate.Input, session *acpSession) string {
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
	if !session.answer.seen || !session.answer.nonblank {
		return refusalEmpty
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
