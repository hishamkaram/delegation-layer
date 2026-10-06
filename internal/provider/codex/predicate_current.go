package codex

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func evaluateCurrentInterpreter(v interpreter, input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	return evaluateCurrentInterpreterAgainst(v, input, raw, out, v.Reference())
}

func evaluateCurrentInterpreterAgainst(v interpreter, input predicate.Input, raw predicate.Evidence, out io.Writer, reference task.PredicateRef) (task.Interpretation, error) {
	if err := validateCurrentEvaluationInput(input, raw, out, reference); err != nil {
		return task.Interpretation{}, err
	}
	state, err := readCurrentEvidence(raw, v.historicalCurrent)
	if err != nil {
		return task.Interpretation{}, err
	}
	usage := currentUsageMetadata(state, task.UsageReliabilityUnreliable)
	if refusal := currentRefusalFor(input, state, v.provider()); refusal != "" {
		result := rejectedWithUsage(refusal, usage)
		result.Diagnostics = append(result.Diagnostics, currentDiagnostic(state.semantic, refusal))
		result.Diagnostics = append(result.Diagnostics, currentUsageWarning(state)...)
		return result, nil
	}
	var artifact currentDigest
	var present bool
	if v.provider() == Provider {
		artifact, present, err = readCurrentArtifact(raw)
		if err != nil {
			return task.Interpretation{}, err
		}
	}
	finalDigest, err := streamCurrentFinal(raw, state.final, io.Discard)
	if err != nil {
		return task.Interpretation{}, err
	}
	if present && (artifact.length == 0 || !digestsEqual(artifact, finalDigest)) {
		result := rejectedWithUsage(refusalOutputConflict, usage)
		result.Diagnostics = append(result.Diagnostics, currentDiagnostic(nil, refusalOutputConflict))
		result.Diagnostics = append(result.Diagnostics, currentUsageWarning(state)...)
		return result, nil
	}
	if _, err = streamCurrentFinal(raw, state.final, out); err != nil {
		return task.Interpretation{}, err
	}
	session := task.SessionIdentity{Provider: v.provider(), ConversationID: state.threadID}
	result := task.Interpretation{
		Verdict: task.VerdictCommitted,
		Session: &session,
		Usage:   currentUsageMetadata(state, task.UsageReliabilityReported),
	}
	result.Diagnostics = currentUsageWarning(state)
	return result, nil
}

func currentUsageWarning(state currentEventState) []task.OutputDiagnostic {
	if state.usage == nil || !state.usage.present || state.usageReliable {
		return nil
	}
	return []task.OutputDiagnostic{{
		Code:    "optional_usage_unavailable",
		Message: "Optional Codex usage could not be interpreted; the answer was assessed independently.",
		Field:   "usage",
	}}
}

func currentDiagnostic(parseErr error, refusal string) task.OutputDiagnostic {
	diagnostic := task.OutputDiagnostic{
		Code:    "provider_output_rejected",
		Message: "Codex output did not satisfy the recorded completion contract.",
	}
	switch {
	case refusal == refusalMalformed:
		diagnostic.Code = "invalid_output"
		diagnostic.Message = "Codex output contained invalid completion metadata."
	case refusal == refusalIdentity:
		diagnostic.Code = "identity_mismatch"
		diagnostic.Message = "Codex conversation identity did not match the task."
	case refusal == refusalIncomplete:
		diagnostic.Code = "incomplete_output"
		diagnostic.Message = "Codex output did not contain a complete successful turn."
	case refusal == refusalEmpty:
		diagnostic.Code = "empty_output"
		diagnostic.Message = "Codex output did not contain a nonblank final answer."
	case strings.HasPrefix(refusal, refusalProviderFailed):
		diagnostic.Code = "provider_failed"
		diagnostic.Message = "Codex reported a failed or interrupted turn."
	case refusal == refusalOutputConflict:
		diagnostic.Code = "output_conflict"
		diagnostic.Message = "Codex final answer and optional output artifact disagreed."
	}
	var fieldError *providerjson.FieldError
	if errors.As(parseErr, &fieldError) && fieldError.Code != "" {
		diagnostic.Code = fieldError.Code
		diagnostic.Field = fieldError.Field
		diagnostic.Offset = fieldError.Offset
	}
	if err := task.ValidateOutputDiagnostic(diagnostic); err != nil {
		diagnostic.Code = "provider_output_rejected"
		diagnostic.Message = "Codex output did not satisfy the recorded completion contract."
		diagnostic.Field = ""
		diagnostic.Offset = 0
	}
	return diagnostic
}

func validateCurrentEvaluationInput(input predicate.Input, raw predicate.Evidence, out io.Writer, reference task.PredicateRef) error {
	if !input.Seal.Predicate.Equal(reference) {
		return fmt.Errorf("%w: current Codex predicate reference mismatch", task.ErrIdentityMismatch)
	}
	if err := task.ValidateProviderExitRecord(&input.Seal); err != nil {
		return err
	}
	if raw == nil {
		return errors.New("nil current Codex evidence")
	}
	if out == nil {
		return errors.New("nil current Codex answer sink")
	}
	return nil
}

// CanRecover reports the same-wire revisions that the corrected current
// interpreter can re-evaluate without changing the original sealed input.
func (v interpreter) CanRecover(source task.PredicateRef) bool {
	if v.legacy || v.historicalCurrent || v.historicalRuntime || v.mode != Mode && v.mode != WorkspaceWriteMode {
		return false
	}
	if source.Equal(historicalCurrentRuntimeReferenceForMode(v.mode)) {
		return true
	}
	if source.Equal(historicalRuntimeReferenceForMode(v.mode)) {
		return true
	}
	return v.mode == Mode && source.Equal(LegacyReference())
}

// Target is the explicit predicate selected by recovery. Historical and
// legacy interpreter values also expose the method so the registry can inspect
// one uniform catalog; CanRecover keeps them ineligible as recovery targets.
func (v interpreter) Target() task.PredicateRef { return v.Reference() }

// Recover re-evaluates the original sealed evidence with the corrected
// selected-field parser. The source predicate in input.Seal is intentionally
// left untouched; the recovery coordinator owns the derived target record.
func (v interpreter) Recover(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if !v.CanRecover(input.Seal.Predicate) {
		return task.Interpretation{}, task.ErrIncompatiblePredicate
	}
	return evaluateCurrentInterpreterAgainst(v, input, raw, out, input.Seal.Predicate)
}

func readCurrentEvidence(raw predicate.Evidence, requireItemStart bool) (currentEventState, error) {
	var state currentEventState
	stdoutErr := raw.Read(predicate.Stdout, func(reader io.Reader) error {
		var err error
		state, err = parseCurrentStdout(reader, requireItemStart)
		return err
	})
	stderrErr := raw.Read(predicate.Stderr, commonprovider.DrainReader)
	return state, errors.Join(stdoutErr, stderrErr)
}

var _ predicate.RecoveryInterpreter = interpreter{}
