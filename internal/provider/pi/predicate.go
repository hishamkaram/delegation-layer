package pi

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
	mode    string
	version string
	legacy  bool
}

// NewInterpreter returns the immutable Pi JSON interpreter for mode. With no
// argument it returns the read-only interpreter for package compatibility.
func NewInterpreter(mode ...string) predicate.Interpreter {
	selected := ModeReadOnly
	if len(mode) > 0 && mode[0] != "" {
		selected = mode[0]
	}
	return interpreter{mode: selected, version: currentPredicateVersion}
}

func newLegacyInterpreter() predicate.Interpreter {
	return interpreter{mode: ModeReadOnly, version: legacyPredicateVersion, legacy: true}
}

func newReadOnlyV2Interpreter() predicate.Interpreter {
	return interpreter{mode: ModeReadOnly, version: historicalPredicateV2}
}

func newHistoricalV3Interpreter(mode string) predicate.Interpreter {
	return interpreter{mode: mode, version: historicalPredicateV3}
}

func newHistoricalV4Interpreter(mode string) predicate.Interpreter {
	return interpreter{mode: mode, version: predicateVersion}
}

func (i interpreter) Reference() task.PredicateRef {
	return referenceForVersion(i.mode, i.version)
}

func (i interpreter) Evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	return i.evaluateAgainst(input, raw, out, i.Reference())
}

func (i interpreter) evaluateAgainst(input predicate.Input, raw predicate.Evidence, out io.Writer, reference task.PredicateRef) (task.Interpretation, error) {
	if !validMode(i.mode) {
		return task.Interpretation{}, fmt.Errorf("%w: invalid Pi interpreter mode", task.ErrIdentityMismatch)
	}
	if err := validateEvaluationInput(input, raw, out, reference); err != nil {
		return task.Interpretation{}, err
	}
	nativeRetryContract := i.version == predicateVersion || i.version == historicalPredicateV3
	var stdout eventState
	var err error
	if i.version == currentPredicateVersion {
		stdout, err = readCurrentEvidence(raw)
	} else {
		stdout, err = readEvidenceWithOptions(raw, parserOptions{
			allowNativeRetry:          nativeRetryContract,
			settledTerminal:           nativeRetryContract,
			rejectTrailingRetryEnd:    i.version == predicateVersion,
			requireAgentEndErrorMatch: i.version == predicateVersion,
		})
	}
	if err != nil {
		return task.Interpretation{}, err
	}
	if refusal := refusalFor(input, stdout); refusal != "" {
		result := task.Interpretation{Verdict: task.VerdictRejected, Refusal: refusal, Usage: currentUsageFromState(stdout, task.UsageReliabilityUnreliable)}
		if stdout.current {
			result.Diagnostics = append(result.Diagnostics, piCurrentDiagnostic(refusal))
			result.Diagnostics = append(result.Diagnostics, piCurrentUsageWarning(stdout)...)
		}
		return result, nil
	}
	if stdout.current {
		if err = raw.Read(predicate.Stdout, func(reader io.Reader) error {
			return copyPiCurrentMessageText(reader, stdout.currentTurnMessageStart, stdout.currentTurnMessageEnd, out)
		}); err != nil {
			return task.Interpretation{}, err
		}
	} else if err = writeAnswer(out, stdout.turnText); err != nil {
		return task.Interpretation{}, err
	}
	session := task.SessionIdentity{Provider: Provider, ConversationID: stdout.sessionID}
	result := task.Interpretation{
		Verdict: task.VerdictCommitted,
		Session: &session,
		Usage:   currentUsageFromState(stdout, task.UsageReliabilityReported),
	}
	if stdout.current {
		result.Diagnostics = piCurrentUsageWarning(stdout)
	}
	return result, nil
}

func validateEvaluationInput(input predicate.Input, raw predicate.Evidence, out io.Writer, reference task.PredicateRef) error {
	if !input.Seal.Predicate.Equal(reference) {
		return fmt.Errorf("%w: Pi predicate reference mismatch", task.ErrIdentityMismatch)
	}
	if err := task.ValidateProviderExitRecord(&input.Seal); err != nil {
		return err
	}
	if raw == nil {
		return errors.New("nil Pi evidence")
	}
	if out == nil {
		return errors.New("nil Pi answer sink")
	}
	return nil
}

// Target is the explicit current predicate selected by recovery. Historical
// interpreter values expose it as well, but CanRecover keeps them replay-only.
func (i interpreter) Target() task.PredicateRef { return i.Reference() }

// CanRecover permits the corrected parser to reassess the immediately prior
// native revision for the same Pi permission mode. Older policy revisions are
// deliberately not treated as interchangeable evidence.
func (i interpreter) CanRecover(source task.PredicateRef) bool {
	return !i.legacy && i.version == currentPredicateVersion && source.Equal(referenceForVersion(i.mode, predicateVersion))
}

func (i interpreter) Recover(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if !i.CanRecover(input.Seal.Predicate) {
		return task.Interpretation{}, task.ErrIncompatiblePredicate
	}
	return i.evaluateAgainst(input, raw, out, input.Seal.Predicate)
}

func readEvidence(raw predicate.Evidence) (eventState, error) {
	return readEvidenceWithOptions(raw, parserOptions{})
}

func readEvidenceWithOptions(raw predicate.Evidence, options parserOptions) (eventState, error) {
	var stdout eventState
	stdoutErr := raw.Read(predicate.Stdout, func(reader io.Reader) error {
		var err error
		stdout, err = parseHistoricalStdout(reader, options)
		return err
	})
	stderrErr := raw.Read(predicate.Stderr, commonprovider.DrainReader)
	return stdout, errors.Join(stdoutErr, stderrErr)
}

func readCurrentEvidence(raw predicate.Evidence) (eventState, error) {
	var stdout eventState
	stdoutErr := raw.Read(predicate.Stdout, func(reader io.Reader) error {
		var err error
		stdout, err = parseStdout(reader)
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
	return refusalForCompletedCycle(input, state)
}

func refusalForCompletedCycle(input predicate.Input, state eventState) string {
	if !state.sessionSeen || !state.agentStarted || !state.turnStarted {
		return refusalMalformed
	}
	if !identityMatches(input, state.sessionID) {
		return refusalIdentity
	}
	if state.retryPending || !state.turnEnded || !state.agentEnded {
		return refusalIncomplete
	}
	if state.current {
		return currentCycleAnswerRefusal(state)
	}
	if historicalCycleAnswerEmpty(state) {
		return refusalEmpty
	}
	return ""
}

func currentCycleAnswerRefusal(state eventState) string {
	if !state.currentTurnText.nonblank || !state.currentAgentText.nonblank {
		return refusalEmpty
	}
	return ""
}

func historicalCycleAnswerEmpty(state eventState) bool {
	return len(bytes.TrimSpace(state.turnText)) == 0 || len(bytes.TrimSpace(state.agentText)) == 0
}

func currentUsageFromState(state eventState, reliability string) []task.UsageMetadata {
	if !state.current {
		return usageFromState(state, reliability)
	}
	if !state.currentUsageReliable {
		reliability = task.UsageReliabilityUnreliable
	}
	return usageFromState(state, reliability)
}

func piCurrentUsageWarning(state eventState) []task.OutputDiagnostic {
	if !state.current || !state.currentUsagePresent || state.currentUsageReliable {
		return nil
	}
	return []task.OutputDiagnostic{{
		Code:    "optional_usage_unavailable",
		Message: "Optional Pi usage could not be interpreted; the answer was assessed independently.",
		Field:   "usage",
	}}
}

func piCurrentDiagnostic(refusal string) task.OutputDiagnostic {
	diagnostic := task.OutputDiagnostic{
		Code:    "provider_output_rejected",
		Message: "Pi output did not satisfy the recorded completion contract.",
	}
	switch {
	case refusal == refusalMalformed:
		diagnostic.Code = "invalid_output"
		diagnostic.Message = "Pi output contained invalid completion metadata."
	case refusal == refusalIdentity:
		diagnostic.Code = "identity_mismatch"
		diagnostic.Message = "Pi session identity did not match the task."
	case refusal == refusalIncomplete:
		diagnostic.Code = "incomplete_output"
		diagnostic.Message = "Pi output did not contain a complete successful cycle."
	case refusal == refusalEmpty:
		diagnostic.Code = "empty_output"
		diagnostic.Message = "Pi output did not contain a nonblank final answer."
	case strings.HasPrefix(refusal, refusalProviderFailed):
		diagnostic.Code = "provider_failed"
		diagnostic.Message = "Pi reported a failed or interrupted completion."
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

func writeAnswer(out io.Writer, data []byte) error {
	if out == nil {
		return errors.New("nil Pi answer sink")
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

var _ predicate.Interpreter = interpreter{}
