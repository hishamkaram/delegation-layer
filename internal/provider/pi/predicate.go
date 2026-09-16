package pi

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

// NewInterpreter returns the immutable Pi JSON interpreter for mode. With no
// argument it returns the read-only interpreter for package compatibility.
func NewInterpreter(mode ...string) predicate.Interpreter {
	selected := ModeReadOnly
	if len(mode) > 0 && mode[0] != "" {
		selected = mode[0]
	}
	return interpreter{mode: selected}
}

func (i interpreter) Reference() task.PredicateRef { return ReferenceForMode(i.mode) }

func (i interpreter) Evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if !validMode(i.mode) {
		return task.Interpretation{}, fmt.Errorf("%w: invalid Pi interpreter mode", task.ErrIdentityMismatch)
	}
	if err := validateEvaluationInput(input, raw, out, i.mode); err != nil {
		return task.Interpretation{}, err
	}
	stdout, err := readEvidence(raw)
	if err != nil {
		return task.Interpretation{}, err
	}
	usage := usageFromState(stdout, task.UsageReliabilityUnreliable)
	if refusal := refusalFor(input, stdout); refusal != "" {
		return task.Interpretation{Verdict: task.VerdictRejected, Refusal: refusal, Usage: usage}, nil
	}
	if err = writeAnswer(out, stdout.turnText); err != nil {
		return task.Interpretation{}, err
	}
	session := task.SessionIdentity{Provider: Provider, ConversationID: stdout.sessionID}
	return task.Interpretation{
		Verdict: task.VerdictCommitted,
		Session: &session,
		Usage:   usageFromState(stdout, task.UsageReliabilityReported),
	}, nil
}

func validateEvaluationInput(input predicate.Input, raw predicate.Evidence, out io.Writer, mode string) error {
	if !input.Seal.Predicate.Equal(ReferenceForMode(mode)) {
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
	if !state.sessionSeen || !state.agentStarted || !state.turnStarted {
		return refusalMalformed
	}
	if !identityMatches(input, state.sessionID) {
		return refusalIdentity
	}
	if !state.turnEnded || !state.agentEnded {
		return refusalIncomplete
	}
	if len(bytes.TrimSpace(state.turnText)) == 0 || len(bytes.TrimSpace(state.agentText)) == 0 {
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
