package claude

import (
	"errors"
	"io"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const historicalInteractionContract = "Claude stream-json host clarification v1: one initial user input; exact native session; repeated same-session initialization permitted before completion; AskUserQuestion answers only; never authorize other tools; native result plus idle and settled tracked agents; fresh root result after background completion; earlier provider errors remain fatal; latest successful nonempty root result; zero exit; malformed, incomplete or conflicting evidence rejects; control journal sealed with raw streams\n"

const previousInteractionContract = "Claude stream-json host clarification v2: one initial user input; exact native session; repeated same-session initialization permitted before completion; AskUserQuestion answers only; never authorize other tools; native result plus idle and settled tracked agents; fresh root result after background completion; earlier provider errors remain fatal; latest successful nonempty root result; offset-streamed sealed records without a native event-size ceiling; malformed, incomplete or conflicting evidence rejects; control journal sealed with raw streams\n"

const preAttributionInteractionContract = "Claude stream-json host clarification v3: one initial user input; exact native session; repeated same-session initialization permitted before completion; AskUserQuestion answers only; never authorize other tools; native result plus idle and settled tracked agents; fresh root result after background completion; earlier provider errors remain fatal; latest successful nonempty root result; offset-streamed sealed records without a native event-size ceiling; stable runtime probe requires only --print; malformed, incomplete or conflicting evidence rejects; control journal sealed with raw streams\n"

// unscopedInteractionContract is the frozen interactive-v4 revision. Its bytes
// and its unscoped attribution policy, which opens a root follow-up cycle for
// every assistant or user message after a result, are retained for exact replay
// and reconstruction of tasks admitted under it; its "current native print
// contract" refers to the print contract at that admission. Its end-of-stream
// behavior is retained as admitted: live completion keeps sticky input closure;
// sealed replay applies the reducer's settlement, with no idle re-check at EOF.
const unscopedInteractionContract = "Claude stream-json host clarification v4: one initial user input; exact native session; repeated same-session initialization permitted before the principal result; AskUserQuestion answers only; never authorize other tools; input closes after the principal result, idle session, settled tracked agents, and settled follow-up cycles and batches; provider-native follow-ups after input closure remain evidence; completion attribution follows the current native print contract: the first result answers the caller turn with origin absent or human and result_index 0 when present, later results need consecutive result_index and origin task-notification without subkind, terminal_reason when present is completed, queued_turn_count when present is 0, an empty follow-up settles only as a documented zero-turn batch acknowledgement before its answering result, and follow-up failures, refusals, or unsettled cycles reject; only the principal result is selected; offset-streamed sealed records without a native event-size ceiling; stable runtime probe requires only --print; malformed, incomplete or conflicting evidence rejects; control journal sealed with raw streams\n"

// scopedInteractionContract is the frozen interactive-v5 revision bound to the
// scoped completion reducer and strict wire of print evidence at its
// admission. Its bytes are retained for exact replay and reconstruction.
const scopedInteractionContract = "Claude stream-json host clarification v5: one initial user input; exact native session; one initial system/init, with one identical repeated init permitted after each leading acknowledgement before root messages and, after the principal result, only at the start of a follow-up cycle before that cycle's root messages; AskUserQuestion answers only; never authorize other tools; a root tool_result resolves a pending question and a nested agent tool_result never does; input closes after the principal result, idle session, settled tracked agents, and settled follow-up cycles and batches, the closure is sticky, and end of stream requires the current session to be idle with no unsettled tracked agent, question, cycle, or batch; provider-native follow-ups after input closure remain evidence; completion attribution: the first result that is not a leading acknowledgement answers the caller turn with origin absent or human and result_index 0 when present, or the next consecutive index after leading acknowledgements, later results need consecutive result_index and origin task-notification without subkind, terminal_reason when present is completed, queued_turn_count when present is 0, an empty follow-up settles only as a documented zero-turn batch acknowledgement before its answering result, and follow-up failures, refusals, or unsettled cycles reject; leading acknowledgements: before the caller's result, results with origin task-notification without subkind that are successful, have an empty string result, num_turns explicitly 0, and consecutive result_index from 0, placed after the initial init and before any root message since that init or the previous acknowledgement, are accepted; they never answer, contribute usage or cost, or close the input, a failed, refused, aborted, or queued one always rejects, with its canonical refusal unless a higher-priority malformed or incomplete fault takes precedence, a stream of only acknowledgements is incomplete, and any other shape is malformed; message scope: a complete assistant or user message whose parent_tool_use_id is absent or null is a root message and, after a result, opens a follow-up cycle that its attributed result must close; a message whose parent_tool_use_id is a nonblank string of at most 1024 bytes without NUL or invalid UTF-8 is a nested agent message that stays structurally validated and session-bound but never opens, advances, or settles a cycle or batch, resolves a pending question, is selected as an answer, or affects accounting; any other parent_tool_use_id value, including an empty, blank, oversized, mistyped, or case-aliased field, is malformed before or after the principal result; only the principal result is selected; offset-streamed sealed records without a native event-size ceiling; stable runtime probe requires only --print; malformed, incomplete or conflicting evidence rejects; control journal sealed with raw streams\n"

// advisoryInteractionContract is the frozen principal-only interactive-v6
// revision: the frozen interactive-v5 bytes with advisoryInteractionEdits
// applied. It is replay-only.
func advisoryInteractionContract() string {
	return applyContractEdits(scopedInteractionContract, advisoryInteractionEdits())
}

// interactionContract preserves the interactive-v7 follow-up policy for
// replay. Its text is the frozen interactive-v6 bytes with
// followUpInteractionEdits applied; the authentication revision derives from it.
func interactionContract() string {
	return applyContractEdits(advisoryInteractionContract(), followUpInteractionEdits())
}

// followUpInteractionEdits derive interactive-v7 from the frozen
// interactive-v6 bytes: the revision label and the shared follow-up selection
// and failed-request policy in place of principal-only selection.
func followUpInteractionEdits() []contractEdit {
	return []contractEdit{
		{"host clarification v6:", "host clarification v7:"},
		{"; only the principal result is selected; advisory wire:", "; " + followUpSelectionPolicy + "; advisory wire:"},
	}
}

// advisoryInteractionEdits are the ordered clause edits that derive the
// interactive-v6 contract from the frozen interactive-v5 bytes: the revision
// label, the repeated-init authority, unsupported completion reasons, and the
// advisory wire with optional task_type.
func advisoryInteractionEdits() []contractEdit {
	return []contractEdit{
		{"host clarification v5:", "host clarification v6:"},
		{"with one identical repeated init permitted", "with one repeated init of the same session and native permission mode permitted"},
		{
			"terminal_reason when present is completed,",
			"terminal_reason when present is completed, a documented failure reason rejects, any other terminal_reason or stop_reason string of at most 128 bytes is an unsupported completion that rejects as unsupported-completion and never as success, provider failure, or malformed output,",
		},
		{
			"; only the principal result is selected; offset-streamed",
			"; only the principal result is selected; advisory wire: session_id and the requested native permissionMode are the only init authority and other init metadata is advisory, never defaulted, and may change on a repeated init; selected authority and control fields must be unique, including case-folded aliases, and host control records stay unique as a whole, while opaque payloads are validated as JSON syntax only and unknown content block types are opaque; system/task_started without task_type is untracked; offset-streamed",
		},
	}
}

type interactionInterpreter struct {
	mode       string
	historical bool
	reference  *task.PredicateRef
	// completion is the reducer bound to this revision's identity: the current
	// revision diagnoses authentication and projects follow-ups, interactive-v7
	// keeps its follow-up policy, interactive-v6 keeps its principal-only
	// advisory policy, interactive-v5 keeps its scoped attribution,
	// interactive-v4 its unscoped attribution, and the zero value is the frozen
	// single-result policy.
	completion completionPolicy
}

func newInteractionInterpreter(mode string) predicate.Interpreter {
	return interactionInterpreter{mode: mode, completion: authenticationFollowUpCompletion}
}

func interactionReference(mode string) task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: "interactive-v8", SHA256: task.ComputeSHA256([]byte(authenticationInteractionContract() + mode + "\n"))}
}

// advisoryInteractionReference is the frozen interactive-v6 revision, which
// keeps its principal-only advisory completion for exact replay.
func advisoryInteractionReference(mode string) task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: "interactive-v6", SHA256: task.ComputeSHA256([]byte(advisoryInteractionContract() + mode + "\n"))}
}

func newAdvisoryInteractionInterpreter(mode string) predicate.Interpreter {
	reference := advisoryInteractionReference(mode)
	return interactionInterpreter{mode: mode, reference: &reference, completion: advisoryScopedCompletion}
}

// scopedInteractionReference is the frozen interactive-v5 revision, which
// keeps its scoped attributed completion and strict wire for exact replay.
func scopedInteractionReference(mode string) task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: "interactive-v5", SHA256: task.ComputeSHA256([]byte(scopedInteractionContract + mode + "\n"))}
}

// unscopedInteractionReference is the frozen interactive-v4 revision, which
// keeps its unscoped attributed completion for exact replay.
func unscopedInteractionReference(mode string) task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: "interactive-v4", SHA256: task.ComputeSHA256([]byte(unscopedInteractionContract + mode + "\n"))}
}

// preAttributionInteractionReference is the frozen interactive-v3 revision,
// which keeps its latest-successful-result selection for sealed replay.
func preAttributionInteractionReference(mode string) task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: "interactive-v3", SHA256: task.ComputeSHA256([]byte(preAttributionInteractionContract + mode + "\n"))}
}

func previousInteractionReference(mode string) task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: "interactive-v2", SHA256: task.ComputeSHA256([]byte(previousInteractionContract + mode + "\n"))}
}

func historicalInteractionReference(mode string) task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: "interactive-v1", SHA256: task.ComputeSHA256([]byte(historicalInteractionContract + mode + "\n"))}
}

func newScopedInteractionInterpreter(mode string) predicate.Interpreter {
	reference := scopedInteractionReference(mode)
	return interactionInterpreter{mode: mode, reference: &reference, completion: scopedAttributedCompletion}
}

func newUnscopedInteractionInterpreter(mode string) predicate.Interpreter {
	reference := unscopedInteractionReference(mode)
	return interactionInterpreter{mode: mode, reference: &reference, completion: unscopedAttributedCompletion}
}

func newPreviousInteractionInterpreter(mode string) predicate.Interpreter {
	reference := previousInteractionReference(mode)
	return interactionInterpreter{mode: mode, reference: &reference}
}

func newPreAttributionInteractionInterpreter(mode string) predicate.Interpreter {
	reference := preAttributionInteractionReference(mode)
	return interactionInterpreter{mode: mode, reference: &reference}
}

func (v interactionInterpreter) Reference() task.PredicateRef {
	if v.reference != nil {
		return *v.reference
	}
	if v.historical {
		return historicalInteractionReference(v.mode)
	}
	return interactionReference(v.mode)
}

func (v interactionInterpreter) Evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	return v.evaluateAgainst(input, raw, out, v.Reference())
}

func (v interactionInterpreter) evaluateAgainst(input predicate.Input, raw predicate.Evidence, out io.Writer, reference task.PredicateRef) (task.Interpretation, error) {
	ref := v.Reference()
	if err := validateEvaluationInput(interpreter{mode: v.mode, streamReference: &ref}, input, raw, out, reference); err != nil {
		return task.Interpretation{}, err
	}
	p := newSealedClaudeInteraction(v, input)
	semantic, readErr := v.readInteractionStdout(raw, p)
	if err := errors.Join(readErr, raw.Read(predicate.Stderr, commonprovider.DrainReader)); err != nil {
		return task.Interpretation{}, err
	}
	if p.state.attribution == nil {
		// Frozen revisions publish the latest root result after settlement.
		p.state.resultSeen = p.resultSeen
	} else {
		p.state.attribution.finish(&p.state)
	}
	if !interactionStreamComplete(semantic, p.Complete()) {
		return v.incompleteInteractionResult(input, p), nil
	}
	if refusal := refusalFor(input, p.state); refusal != "" {
		return v.refusedInteractionResult(input, refusal, p), nil
	}
	if err := v.writeInteractionAnswer(raw, p, out); err != nil {
		return task.Interpretation{}, err
	}
	return v.committedInteractionResult(p), nil
}

func newSealedClaudeInteraction(v interactionInterpreter, input predicate.Input) *claudeInteraction {
	p := &claudeInteraction{state: eventState{mode: v.mode, current: !v.historical, attribution: newCompletionAttribution(v.completion)}, turnID: "sealed", pending: map[string]claudePendingQuestion{}, agents: map[string]bool{}}
	if input.RecordedSession != nil {
		p.expected = input.RecordedSession.ConversationID
	} else if input.ExpectedSession.Required {
		p.expected = input.ExpectedSession.ID
	}
	return p
}

func (v interactionInterpreter) readInteractionStdout(raw predicate.Evidence, p *claudeInteraction) (error, error) {
	if v.historical {
		return v.readHistoricalInteractionStdout(raw, p)
	}
	return v.readCurrentInteractionStdout(raw, p)
}

func (v interactionInterpreter) readHistoricalInteractionStdout(raw predicate.Evidence, p *claudeInteraction) (error, error) {
	var semantic error
	readErr := raw.Read(predicate.Stdout, func(reader io.Reader) error {
		var parserErr error
		semantic, parserErr = commonprovider.ReadJSONL(reader, maxEventLineBytes, func(line []byte) error {
			event, err := decodeEvent(line)
			if err != nil {
				return err
			}
			if event.typeName == "system" && p.expected == "" {
				p.expected, _, _ = readOptionalString(event.fields, "session_id")
			}
			_, err = p.Receive(line)
			return err
		})
		return parserErr
	})
	return semantic, readErr
}

func (v interactionInterpreter) readCurrentInteractionStdout(raw predicate.Evidence, p *claudeInteraction) (error, error) {
	var semantic error
	readErr := raw.Read(predicate.Stdout, func(reader io.Reader) error {
		var parserErr error
		semantic, parserErr = providerjson.ReadRecords(reader, func(value providerjson.Value, _ uint64) error {
			if p.expected == "" {
				if event, err := p.state.attribution.decodeEvent(value); err == nil && event.typeName == "system" {
					if session, sessionErr := claudeCurrentRequiredString(event.fields, "session_id", 128); sessionErr == nil {
						p.expected = session
					}
				}
			}
			_, err := p.ReceiveRecord(value)
			return err
		})
		return parserErr
	})
	return semantic, readErr
}

func (v interactionInterpreter) writeInteractionAnswer(raw predicate.Evidence, p *claudeInteraction, out io.Writer) error {
	if !v.historical {
		return raw.Read(predicate.Stdout, func(reader io.Reader) error {
			return writeClaudeCurrentAnswer(reader, p.state, v.completion, out)
		})
	}
	return writeAnswer(out, []byte(p.state.result.answer))
}

func (v interactionInterpreter) incompleteInteractionResult(input predicate.Input, p *claudeInteraction) task.Interpretation {
	// Without a caller result, only a failed leading acknowledgement of the
	// scoped policy can have recorded a refusal; keep its canonical class.
	if refusal := p.state.attribution.completionRefusal(); !p.state.resultSeen && refusal != "" {
		return v.refusedInteractionResult(input, refusal, p)
	}
	result := rejectedWithUsage(refusalIncomplete, nil)
	if !v.historical {
		result.Diagnostics = append(result.Diagnostics, claudeCurrentDiagnostic(refusalIncomplete))
		result.Diagnostics = append(result.Diagnostics, claudeCurrentUsageWarning(p.state)...)
	}
	return result
}

func (v interactionInterpreter) refusedInteractionResult(input predicate.Input, refusal string, p *claudeInteraction) task.Interpretation {
	result := rejectedWithUsage(refusal, claudeCurrentUsageMetadata(p.state, task.UsageReliabilityUnreliable))
	if !v.historical {
		result.Diagnostics = append(result.Diagnostics, claudeRejectionDiagnostic(input, p.state, refusal, Provider))
		result.Diagnostics = append(result.Diagnostics, claudeCurrentUsageWarning(p.state)...)
	}
	return result
}

func (v interactionInterpreter) committedInteractionResult(p *claudeInteraction) task.Interpretation {
	result := task.Interpretation{Verdict: task.VerdictCommitted, Session: &task.SessionIdentity{Provider: Provider, ConversationID: p.expected}, Usage: claudeCurrentUsageMetadata(p.state, task.UsageReliabilityReported)}
	if !v.historical {
		result.Diagnostics = claudeCurrentUsageWarning(p.state)
	}
	return result
}

func (v interactionInterpreter) Target() task.PredicateRef { return v.Reference() }

// CanRecover makes the current follow-up interactive parser the single
// recovery target for every earlier interactive revision of this mode whose
// rejected evidence it can accept. The frozen principal-only interactive-v6
// shares the current verdict rules, so it is never a source.
func (v interactionInterpreter) CanRecover(source task.PredicateRef) bool {
	if v.historical || v.reference != nil {
		return false
	}
	return source.Equal(historicalInteractionReference(v.mode)) || source.Equal(previousInteractionReference(v.mode)) ||
		source.Equal(preAttributionInteractionReference(v.mode)) || source.Equal(unscopedInteractionReference(v.mode)) ||
		source.Equal(scopedInteractionReference(v.mode))
}

func (v interactionInterpreter) Recover(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if !v.CanRecover(input.Seal.Predicate) {
		return task.Interpretation{}, task.ErrIncompatiblePredicate
	}
	return v.evaluateAgainst(input, raw, out, input.Seal.Predicate)
}

func interactionStreamComplete(semantic, completion error) bool {
	return semantic == nil && completion == nil
}
