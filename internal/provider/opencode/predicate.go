package opencode

import (
	"bytes"
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
	refusalProviderFailed = "provider-failed"
)

type interpreter struct {
	mode                string
	legacy              bool
	historicalCurrent   bool
	historicalCurrentV4 bool
	historicalCurrentV3 bool
}

// NewInterpreter returns an OpenCode JSONL interpreter. With no argument it
// returns the default workspace-write profile for compatibility with other
// single-mode provider packages.
func NewInterpreter(modes ...string) predicate.Interpreter {
	mode := ModeWorkspaceWrite
	if len(modes) > 0 {
		mode = modes[0]
	}
	return interpreter{mode: mode}
}

func newLegacyInterpreter(mode string) predicate.Interpreter {
	return interpreter{mode: mode, legacy: true}
}

func newHistoricalCurrentInterpreter(mode string) predicate.Interpreter {
	return interpreter{mode: mode, historicalCurrent: true}
}

func newHistoricalCurrentV3Interpreter(mode string) predicate.Interpreter {
	return interpreter{mode: mode, historicalCurrentV3: true}
}

func newHistoricalCurrentV4Interpreter(mode string) predicate.Interpreter {
	return interpreter{mode: mode, historicalCurrentV4: true}
}

func (v interpreter) Reference() task.PredicateRef {
	if v.legacy {
		return LegacyReferenceForMode(v.mode)
	}
	if v.historicalCurrentV3 {
		return historicalCurrentReferenceForMode(v.mode)
	}
	if v.historicalCurrentV4 {
		return historicalCurrentV4ReferenceForMode(v.mode)
	}
	if v.historicalCurrent {
		return historicalReferenceForMode(v.mode)
	}
	return ReferenceForMode(v.mode)
}

func (v interpreter) Evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	return v.evaluateAgainst(input, raw, out, v.Reference())
}

func (v interpreter) evaluateAgainst(input predicate.Input, raw predicate.Evidence, out io.Writer, reference task.PredicateRef) (task.Interpretation, error) {
	if v.Reference() == (task.PredicateRef{}) {
		return task.Interpretation{}, fmt.Errorf("%w: unsupported OpenCode mode", task.ErrIdentityMismatch)
	}
	if err := validateEvaluationInput(input, raw, out, reference); err != nil {
		return task.Interpretation{}, err
	}
	state, err := readEvidence(raw, v.legacy || v.historicalCurrent, v.historicalCurrentV3, v.historicalCurrentV4)
	if err != nil {
		return task.Interpretation{}, err
	}
	if refusal := refusalFor(input, state); refusal != "" {
		result := rejectedWithUsage(refusal, currentUsageFromTotals(state, task.UsageReliabilityUnreliable))
		if state.current {
			result.Diagnostics = append(result.Diagnostics, openCodeCurrentDiagnostic(refusal))
			result.Diagnostics = append(result.Diagnostics, openCodeCurrentUsageWarning(state)...)
		}
		return result, nil
	}
	if state.current {
		if err = raw.Read(predicate.Stdout, func(reader io.Reader) error {
			return copyOpenCodeCurrentAnswer(reader, state.currentTerminalStart, state.currentTerminalEnd, out)
		}); err != nil {
			return task.Interpretation{}, err
		}
	} else if err = writeAnswer(out, state.answer); err != nil {
		return task.Interpretation{}, err
	}
	session := task.SessionIdentity{Provider: Provider, ConversationID: state.sessionID}
	result := task.Interpretation{
		Verdict: task.VerdictCommitted,
		Session: &session,
		Usage:   currentUsageFromTotals(state, task.UsageReliabilityReported),
	}
	if state.current {
		result.Diagnostics = openCodeCurrentUsageWarning(state)
	}
	return result, nil
}

// Target is the explicit current predicate selected by recovery. Historical
// interpreter values expose it too so the registry can inspect one catalog.
func (v interpreter) Target() task.PredicateRef { return v.Reference() }

// CanRecover permits only the same OpenCode wire and permission-mode
// revisions that preceded the current multi-step parser.
func (v interpreter) CanRecover(source task.PredicateRef) bool {
	if v.legacy || v.historicalCurrent || v.historicalCurrentV3 || v.historicalCurrentV4 {
		return false
	}
	return source.Equal(historicalReferenceForMode(v.mode)) || source.Equal(historicalCurrentReferenceForMode(v.mode)) || source.Equal(historicalCurrentV4ReferenceForMode(v.mode))
}

func (v interpreter) Recover(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if !v.CanRecover(input.Seal.Predicate) {
		return task.Interpretation{}, task.ErrIncompatiblePredicate
	}
	return v.evaluateAgainst(input, raw, out, input.Seal.Predicate)
}

func validateEvaluationInput(input predicate.Input, raw predicate.Evidence, out io.Writer, reference task.PredicateRef) error {
	if !input.Seal.Predicate.Equal(reference) {
		return fmt.Errorf("%w: OpenCode predicate reference mismatch", task.ErrIdentityMismatch)
	}
	if err := task.ValidateProviderExitRecord(&input.Seal); err != nil {
		return err
	}
	if raw == nil {
		return errors.New("nil OpenCode evidence")
	}
	if out == nil {
		return errors.New("nil OpenCode answer sink")
	}
	return nil
}

func readEvidence(raw predicate.Evidence, historical, strictCurrent, historicalCurrentV4 bool) (eventState, error) {
	var stdout eventState
	stdoutErr := raw.Read(predicate.Stdout, func(reader io.Reader) error {
		var err error
		if historical {
			stdout, err = parseHistoricalStdout(reader)
		} else if historicalCurrentV4 {
			stdout, err = parseHistoricalCurrentV4Stdout(reader)
		} else if strictCurrent {
			stdout, err = parseStrictCurrentStdout(reader)
		} else {
			stdout, err = parseStdout(reader)
		}
		return err
	})
	stderrErr := raw.Read(predicate.Stderr, commonprovider.DrainReader)
	return stdout, errors.Join(stdoutErr, stderrErr)
}

func refusalFor(input predicate.Input, state eventState) string {
	if refusal := sealRefusal(input.Seal); refusal != "" {
		return refusal
	}
	if state.semanticFault() {
		return refusalMalformed
	}
	if state.providerFail {
		return refusalProviderFailed
	}
	if !state.sessionSeen || !state.stepStarted {
		return refusalMalformed
	}
	if !identityMatches(input, state.sessionID) {
		return refusalIdentity
	}
	if !state.terminal {
		return refusalIncomplete
	}
	if state.current {
		if !state.currentTerminalText.seen || !state.currentTerminalText.nonblank {
			return refusalEmpty
		}
		return ""
	}
	if !state.terminalAnswerSeen || len(bytes.TrimSpace(state.terminalAnswer)) == 0 {
		return refusalEmpty
	}
	return ""
}

func currentUsageFromTotals(state eventState, reliability string) []task.UsageMetadata {
	if !state.current {
		return usageFromTotals(state.usage, reliability)
	}
	if !state.currentUsageReliable {
		reliability = task.UsageReliabilityUnreliable
	}
	return usageFromTotals(state.usage, reliability)
}

func openCodeCurrentUsageWarning(state eventState) []task.OutputDiagnostic {
	if !state.current || !state.currentUsagePresent || state.currentUsageReliable {
		return nil
	}
	return []task.OutputDiagnostic{{
		Code:    "optional_usage_unavailable",
		Message: "Optional OpenCode usage could not be interpreted; the answer was assessed independently.",
		Field:   "usage",
	}}
}

func openCodeCurrentDiagnostic(refusal string) task.OutputDiagnostic {
	diagnostic := task.OutputDiagnostic{
		Code:    "provider_output_rejected",
		Message: "OpenCode output did not satisfy the recorded completion contract.",
	}
	switch {
	case refusal == refusalMalformed:
		diagnostic.Code = "invalid_output"
		diagnostic.Message = "OpenCode output contained invalid completion metadata."
	case refusal == refusalIdentity:
		diagnostic.Code = "identity_mismatch"
		diagnostic.Message = "OpenCode session identity did not match the task."
	case refusal == refusalIncomplete:
		diagnostic.Code = "incomplete_output"
		diagnostic.Message = "OpenCode output did not contain a complete stopped step."
	case refusal == refusalEmpty:
		diagnostic.Code = "empty_output"
		diagnostic.Message = "OpenCode output did not contain a nonblank final answer."
	case strings.HasPrefix(refusal, refusalProviderFailed):
		diagnostic.Code = "provider_failed"
		diagnostic.Message = "OpenCode reported a failed or interrupted step."
	}
	return diagnostic
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
	if !validSessionID(sessionID) {
		return false
	}
	if input.ExpectedSession.Required {
		if !validSessionID(input.ExpectedSession.ID) || input.ExpectedSession.ID != sessionID {
			return false
		}
	} else if input.ExpectedSession.ID != "" {
		return false
	}
	if input.RecordedSession != nil && (input.RecordedSession.Provider != Provider || input.RecordedSession.ConversationID != sessionID) {
		return false
	}
	return true
}

func rejectedWithUsage(reason string, usage []task.UsageMetadata) task.Interpretation {
	return task.Interpretation{Verdict: task.VerdictRejected, Refusal: reason, Usage: usage}
}

func writeAnswer(out io.Writer, answer []byte) error {
	if out == nil {
		return errors.New("nil OpenCode answer sink")
	}
	for len(answer) > 0 {
		chunk := answer
		if len(chunk) > answerChunkBytes {
			chunk = chunk[:answerChunkBytes]
		}
		written, err := out.Write(chunk)
		if err != nil {
			return err
		}
		if written != len(chunk) {
			return io.ErrShortWrite
		}
		answer = answer[len(chunk):]
	}
	return nil
}
