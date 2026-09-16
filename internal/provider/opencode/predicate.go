package opencode

import (
	"bytes"
	"errors"
	"fmt"
	"io"

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

type interpreter struct{ mode string }

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

func (v interpreter) Reference() task.PredicateRef { return ReferenceForMode(v.mode) }

func (v interpreter) Evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if v.Reference() == (task.PredicateRef{}) {
		return task.Interpretation{}, fmt.Errorf("%w: unsupported OpenCode mode", task.ErrIdentityMismatch)
	}
	if err := validateEvaluationInput(input, raw, out, v.Reference()); err != nil {
		return task.Interpretation{}, err
	}
	state, err := readEvidence(raw)
	if err != nil {
		return task.Interpretation{}, err
	}
	usage := usageFromTotals(state.usage, task.UsageReliabilityUnreliable)
	if refusal := refusalFor(input, state); refusal != "" {
		return rejectedWithUsage(refusal, usage), nil
	}
	if err = writeAnswer(out, state.answer); err != nil {
		return task.Interpretation{}, err
	}
	session := task.SessionIdentity{Provider: Provider, ConversationID: state.sessionID}
	return task.Interpretation{
		Verdict: task.VerdictCommitted,
		Session: &session,
		Usage:   usageFromTotals(state.usage, task.UsageReliabilityReported),
	}, nil
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

func readEvidence(raw predicate.Evidence) (eventState, error) {
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
	if !state.sessionSeen || !state.stepStarted {
		return refusalMalformed
	}
	if !identityMatches(input, state.sessionID) {
		return refusalIdentity
	}
	if !state.terminal {
		return refusalIncomplete
	}
	if !state.terminalAnswerSeen || len(bytes.TrimSpace(state.terminalAnswer)) == 0 {
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
