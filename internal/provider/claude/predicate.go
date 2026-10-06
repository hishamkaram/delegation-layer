package claude

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	refusalMalformed      = "invalid-output"
	refusalIdentity       = "identity-mismatch"
	refusalIncomplete     = "incomplete-output"
	refusalEmpty          = "empty-output"
	refusalRefused        = "refusal"
	refusalProviderFailed = "provider-failed"
)

type interpreter struct {
	streamReference         *task.PredicateRef
	recoverySource          *task.PredicateRef
	mode                    string
	legacy                  bool
	legacyPortable          bool
	nativeHistorical        bool
	historicalNativeCurrent bool
}

// NewInterpreter returns the immutable Claude print stream-json interpreter.
// An omitted mode preserves the historical read-only constructor behavior.
func NewInterpreter(modes ...string) predicate.Interpreter {
	mode := Mode
	if len(modes) > 0 {
		mode = modes[0]
	}
	return interpreter{mode: mode}
}

func newLegacyInterpreter() predicate.Interpreter {
	return interpreter{mode: Mode, legacy: true}
}

func newLegacyNativeInterpreter() predicate.Interpreter {
	return interpreter{mode: Mode, nativeHistorical: true}
}

func newLegacyNativeWorkspaceWriteInterpreter() predicate.Interpreter {
	return interpreter{mode: WorkspaceWriteMode, nativeHistorical: true}
}

func newHistoricalNativeInterpreter(mode string) predicate.Interpreter {
	return interpreter{mode: mode, historicalNativeCurrent: true}
}

func newLegacyPortableInterpreter(mode string) predicate.Interpreter {
	return interpreter{mode: mode, legacyPortable: true}
}

// NewWorkspaceWriteInterpreter returns the Claude interpreter bound to the
// native bypassPermissions profile.
func NewWorkspaceWriteInterpreter() predicate.Interpreter {
	return NewInterpreter(WorkspaceWriteMode)
}

func (v interpreter) Reference() task.PredicateRef {
	if v.streamReference != nil {
		return *v.streamReference
	}
	if v.nativeHistorical {
		return legacyNativeReferenceForMode(v.mode)
	}
	if v.historicalNativeCurrent {
		return historicalNativeReferenceForMode(v.mode)
	}
	if v.legacy {
		return LegacyReference()
	}
	if v.legacyPortable {
		return legacyPortableReferenceForMode(v.mode)
	}
	if v.mode == WorkspaceWriteMode {
		return WorkspaceWriteReference()
	}
	return Reference()
}

// Evaluate interprets sealed stdout/stderr only. It does not own capture,
// process lifetime, sealing, or publication, and it returns operational reader
// and answer-writer faults to the core instead of turning them into a refusal.
func (v interpreter) Evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	return v.evaluateAgainst(input, raw, out, v.Reference())
}

func (v interpreter) evaluateAgainst(input predicate.Input, raw predicate.Evidence, out io.Writer, reference task.PredicateRef) (task.Interpretation, error) {
	if err := validateEvaluationInput(v, input, raw, out, reference); err != nil {
		return task.Interpretation{}, err
	}
	state, err := readEvidence(raw, v.mode, v.legacy || v.legacyPortable, v.nativeHistorical, v.historicalNativeCurrent)
	if err != nil {
		return task.Interpretation{}, err
	}
	if refusal := refusalForProvider(input, state, v.Reference().Adapter); refusal != "" {
		result := rejectedWithUsage(refusal, claudeCurrentUsageMetadata(state, task.UsageReliabilityUnreliable))
		if state.current {
			result.Diagnostics = append(result.Diagnostics, claudeCurrentDiagnostic(refusal))
			result.Diagnostics = append(result.Diagnostics, claudeCurrentUsageWarning(state)...)
		}
		return result, nil
	}
	if state.current {
		if err := raw.Read(predicate.Stdout, func(reader io.Reader) error {
			return copyClaudeSpans(reader, state.result.currentAnswer.spans, out)
		}); err != nil {
			return task.Interpretation{}, err
		}
	} else if err := writeAnswer(out, []byte(state.result.answer)); err != nil {
		return task.Interpretation{}, err
	}
	session := task.SessionIdentity{Provider: v.Reference().Adapter, ConversationID: state.result.sessionID}
	result := task.Interpretation{
		Verdict: task.VerdictCommitted,
		Session: &session,
		Usage:   claudeCurrentUsageMetadata(state, task.UsageReliabilityReported),
	}
	if state.current {
		result.Diagnostics = claudeCurrentUsageWarning(state)
	}
	return result, nil
}

func validateEvaluationInput(v interpreter, input predicate.Input, raw predicate.Evidence, out io.Writer, reference task.PredicateRef) error {
	if !input.Seal.Predicate.Equal(reference) {
		return fmt.Errorf("%w: Claude predicate reference mismatch", task.ErrIdentityMismatch)
	}
	if err := task.ValidateProviderExitRecord(&input.Seal); err != nil {
		return err
	}
	if raw == nil {
		return errors.New("nil Claude evidence")
	}
	if out == nil {
		return errors.New("nil Claude answer sink")
	}
	return nil
}

// Target is the explicit current predicate selected by recovery. Historical
// interpreter values expose it too so the registry can inspect one uniform
// catalog, while CanRecover keeps them ineligible as targets.
func (v interpreter) Target() task.PredicateRef { return v.Reference() }

// CanRecover permits only same-wire native revisions for the same permission
// mode. Legacy permission and portable revisions have different policy
// contracts and must remain replay-only.
func (v interpreter) CanRecover(source task.PredicateRef) bool {
	if v.legacy || v.legacyPortable || v.nativeHistorical || v.historicalNativeCurrent {
		return false
	}
	if v.recoverySource != nil {
		return source.Equal(*v.recoverySource)
	}
	return source.Equal(historicalNativeReferenceForMode(v.mode))
}

// Recover re-evaluates the original sealed evidence with the current
// selected-field parser. The source predicate in input.Seal is intentionally
// left untouched; the recovery coordinator owns the derived target record.
func (v interpreter) Recover(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if !v.CanRecover(input.Seal.Predicate) {
		return task.Interpretation{}, task.ErrIncompatiblePredicate
	}
	return v.evaluateAgainst(input, raw, out, input.Seal.Predicate)
}

func readEvidence(raw predicate.Evidence, mode string, strict, nativeHistorical, historicalNativeCurrent bool) (eventState, error) {
	var stdout eventState
	stdoutErr := raw.Read(predicate.Stdout, func(reader io.Reader) error {
		var err error
		if nativeHistorical {
			stdout, err = parseHistoricalNativeStdoutForMode(reader, mode)
		} else if historicalNativeCurrent {
			stdout, err = parseHistoricalCurrentStdoutForMode(reader, mode)
		} else if strict {
			stdout, err = parseLegacyStdoutForMode(reader, mode)
		} else {
			stdout, err = parseStdoutForMode(reader, mode)
		}
		return err
	})
	stderrErr := raw.Read(predicate.Stderr, commonprovider.DrainReader)
	return stdout, errors.Join(stdoutErr, stderrErr)
}

func refusalFor(input predicate.Input, state eventState) string {
	return refusalForProvider(input, state, Provider)
}

func refusalForProvider(input predicate.Input, state eventState, providerID string) string {
	if refusal := sealRefusal(input.Seal); refusal != "" {
		return refusal
	}
	if state.semanticFault() {
		return refusalMalformed
	}
	if !state.initSeen {
		return refusalMalformed
	}
	if !identityMatchesProvider(input, state.init.sessionID, providerID) {
		return refusalIdentity
	}
	if !state.resultSeen {
		return refusalIncomplete
	}
	return resultRefusal(state.result)
}

func resultRefusal(result resultState) string {
	if result.subtype != resultSuccess || result.isError {
		if result.diagnostic == "" {
			return fmt.Sprintf("%s: %s", refusalProviderFailed, result.subtype)
		}
		return fmt.Sprintf("%s: %s: %s", refusalProviderFailed, result.subtype, result.diagnostic)
	}
	if result.diagnostic != "" {
		return fmt.Sprintf("%s: success result includes error diagnostics: %s", refusalProviderFailed, result.diagnostic)
	}
	if result.errorsPresent {
		return refusalProviderFailed + ": success result includes error diagnostics"
	}
	if !result.stopReasonNull && result.stopReasonSeen {
		if refusal := stopReasonRefusal(result.stopReason); refusal != "" {
			return refusal
		}
	}
	if !result.answerSeen {
		return refusalEmpty
	}
	if result.currentAnswerSet {
		if result.currentAnswer.invalidNUL || !result.currentAnswer.nonblank {
			return refusalEmpty
		}
		return ""
	}
	return emptyAnswerRefusal(result.answer)
}

func claudeCurrentUsageMetadata(state eventState, reliability string) []task.UsageMetadata {
	if !state.current {
		return usageFromResult(state, reliability)
	}
	if !state.result.currentUsageReliable {
		reliability = task.UsageReliabilityUnreliable
	}
	return usageFromResult(state, reliability)
}

func claudeCurrentUsageWarning(state eventState) []task.OutputDiagnostic {
	if !state.current || !state.result.currentUsagePresent || state.result.currentUsageReliable {
		return nil
	}
	return []task.OutputDiagnostic{{
		Code:    "optional_usage_unavailable",
		Message: "Optional Claude usage could not be interpreted; the answer was assessed independently.",
		Field:   "usage",
	}}
}

func claudeCurrentDiagnostic(refusal string) task.OutputDiagnostic {
	diagnostic := task.OutputDiagnostic{
		Code:    "provider_output_rejected",
		Message: "Claude output did not satisfy the recorded completion contract.",
	}
	switch {
	case refusal == refusalMalformed:
		diagnostic.Code = "invalid_output"
		diagnostic.Message = "Claude output contained invalid completion metadata."
	case refusal == refusalIdentity:
		diagnostic.Code = "identity_mismatch"
		diagnostic.Message = "Claude conversation identity did not match the task."
	case refusal == refusalIncomplete:
		diagnostic.Code = "incomplete_output"
		diagnostic.Message = "Claude output did not contain a complete successful result."
	case refusal == refusalEmpty:
		diagnostic.Code = "empty_output"
		diagnostic.Message = "Claude output did not contain a nonblank final answer."
	case refusal == refusalRefused:
		diagnostic.Code = "provider_refused"
		diagnostic.Message = "Claude declined or interrupted the requested completion."
	case strings.HasPrefix(refusal, refusalProviderFailed):
		diagnostic.Code = "provider_failed"
		diagnostic.Message = "Claude reported a failed or interrupted completion."
	}
	return diagnostic
}

func stopReasonRefusal(stopReason string) string {
	switch strings.ToLower(stopReason) {
	case "end_turn", "stop_sequence":
		return ""
	case "max_tokens":
		return refusalIncomplete
	case "refusal":
		return refusalRefused
	case "interrupted", "cancel", "cancelled", "canceled", "abort", "aborted":
		return refusalProviderFailed
	default:
		return refusalMalformed
	}
}

func emptyAnswerRefusal(answer string) string {
	if strings.TrimSpace(answer) == "" {
		return refusalEmpty
	}
	return ""
}

func sealRefusal(seal task.ProviderExitRecord) string {
	if seal.InvocationState == task.InvocationStartFailed {
		return refusalProviderFailed + ": start_failed: " + seal.Error
	}
	if seal.Error != "" {
		return refusalProviderFailed + ": seal_error: " + seal.Error
	}
	if seal.ExitCode != 0 {
		return fmt.Sprintf("%s: exit_code=%d", refusalProviderFailed, seal.ExitCode)
	}
	return ""
}

func identityMatches(input predicate.Input, sessionID string) bool {
	return identityMatchesProvider(input, sessionID, Provider)
}

func identityMatchesProvider(input predicate.Input, sessionID, providerID string) bool {
	if !validSessionID(sessionID) {
		return false
	}
	if !sessionMatchesExpectation(input.Seal.RootID, input.Seal.TaskID, input.ExpectedSession, sessionID) {
		return false
	}
	if input.RecordedSession != nil && (input.RecordedSession.Provider != providerID || input.RecordedSession.ConversationID != sessionID) {
		return false
	}
	return true
}

func writeAnswer(out io.Writer, data []byte) error {
	if out == nil {
		return errors.New("nil Claude answer sink")
	}
	for len(data) > 0 {
		chunk := data
		if len(chunk) > answerChunkBytes {
			chunk = chunk[:answerChunkBytes]
		}
		n, err := out.Write(chunk)
		if n < 0 || n > len(chunk) {
			return fmt.Errorf("answer writer returned invalid byte count %d", n)
		}
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func rejectedWithUsage(reason string, usage []task.UsageMetadata) task.Interpretation {
	return task.Interpretation{Verdict: task.VerdictRejected, Refusal: reason, Usage: usage}
}

var _ predicate.Interpreter = interpreter{}
