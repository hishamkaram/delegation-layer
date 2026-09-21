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
	mode             string
	legacy           bool
	legacyPortable   bool
	nativeHistorical bool
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

func newLegacyPortableInterpreter(mode string) predicate.Interpreter {
	return interpreter{mode: mode, legacyPortable: true}
}

// NewWorkspaceWriteInterpreter returns the Claude interpreter bound to the
// native bypassPermissions profile.
func NewWorkspaceWriteInterpreter() predicate.Interpreter {
	return NewInterpreter(WorkspaceWriteMode)
}

func (v interpreter) Reference() task.PredicateRef {
	if v.nativeHistorical {
		return legacyNativeReferenceForMode(v.mode)
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
	if err := validateEvaluationInput(v, input, raw, out); err != nil {
		return task.Interpretation{}, err
	}
	state, err := readEvidence(raw, v.mode, v.legacy || v.legacyPortable, v.nativeHistorical)
	if err != nil {
		return task.Interpretation{}, err
	}
	if refusal := refusalFor(input, state); refusal != "" {
		return rejectedWithUsage(refusal, usageFromResult(state, task.UsageReliabilityUnreliable)), nil
	}
	if err := writeAnswer(out, []byte(state.result.answer)); err != nil {
		return task.Interpretation{}, err
	}
	session := task.SessionIdentity{Provider: Provider, ConversationID: state.result.sessionID}
	return task.Interpretation{
		Verdict: task.VerdictCommitted,
		Session: &session,
		Usage:   usageFromResult(state, task.UsageReliabilityReported),
	}, nil
}

func validateEvaluationInput(v interpreter, input predicate.Input, raw predicate.Evidence, out io.Writer) error {
	if !input.Seal.Predicate.Equal(v.Reference()) {
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

func readEvidence(raw predicate.Evidence, mode string, strict, nativeHistorical bool) (eventState, error) {
	var stdout eventState
	stdoutErr := raw.Read(predicate.Stdout, func(reader io.Reader) error {
		var err error
		if nativeHistorical {
			stdout, err = parseHistoricalNativeStdoutForMode(reader, mode)
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
	if refusal := sealRefusal(input.Seal); refusal != "" {
		return refusal
	}
	if state.semanticFault() {
		return refusalMalformed
	}
	if !state.initSeen {
		return refusalMalformed
	}
	if !identityMatches(input, state.init.sessionID) {
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
	return emptyAnswerRefusal(result.answer)
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
	if !validSessionID(sessionID) {
		return false
	}
	if !sessionMatchesExpectation(input.Seal.RootID, input.Seal.TaskID, input.ExpectedSession, sessionID) {
		return false
	}
	if input.RecordedSession != nil && (input.RecordedSession.Provider != Provider || input.RecordedSession.ConversationID != sessionID) {
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
