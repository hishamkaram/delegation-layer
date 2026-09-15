package codex

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
	refusalOutputConflict = "output-conflict"
	refusalProviderFailed = "provider-failed"
)

type interpreter struct{}

// NewInterpreter returns the immutable Codex exec JSONL interpreter.
func NewInterpreter() predicate.Interpreter { return interpreter{} }

func (interpreter) Reference() task.PredicateRef { return Reference() }

func (interpreter) Evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if err := validateEvaluationInput(input, raw, out); err != nil {
		return task.Interpretation{}, err
	}

	stdout, err := readEvidence(raw)
	if err != nil {
		return task.Interpretation{}, err
	}

	usage := usageFromLastTotal(stdout.usage, task.UsageReliabilityUnreliable)
	if refusal := refusalFor(input, stdout); refusal != "" {
		return rejectedWithUsage(refusal, usage), nil
	}

	artifact, present, oversized, err := readOptionalOutput(raw)
	if err != nil {
		return task.Interpretation{}, err
	}
	if outputConflicts(artifact, present, oversized, stdout.finalMessage) {
		return rejectedWithUsage(refusalOutputConflict, usage), nil
	}
	if err = writeAnswer(out, stdout.finalMessage); err != nil {
		return task.Interpretation{}, err
	}

	session := task.SessionIdentity{Provider: Provider, ConversationID: stdout.threadID}
	return task.Interpretation{
		Verdict: task.VerdictCommitted,
		Session: &session,
		Usage:   usageFromLastTotal(stdout.usage, task.UsageReliabilityReported),
	}, nil
}

func readEvidence(raw predicate.Evidence) (eventState, error) {
	var stdout eventState
	stdoutReadErr := raw.Read(predicate.Stdout, func(reader io.Reader) error {
		var err error
		stdout, err = parseStdout(reader)
		return err
	})
	stderrReadErr := raw.Read(predicate.Stderr, commonprovider.DrainReader)
	return stdout, errors.Join(stdoutReadErr, stderrReadErr)
}

func refusalFor(input predicate.Input, stdout eventState) string {
	if refusal := sealRefusal(input.Seal); refusal != "" {
		return refusal
	}
	if stdout.semanticFault() {
		return refusalMalformed
	}
	if stdout.terminalFailed || stdout.interrupted {
		return refusalProviderFailed
	}
	if !stdout.threadSeen || !stdout.turnStarted {
		return refusalMalformed
	}
	if !identityMatches(input, stdout.threadID) {
		return refusalIdentity
	}
	if !stdout.turnCompleted {
		return refusalIncomplete
	}
	if !stdout.finalSeen || len(bytes.TrimSpace(stdout.finalMessage)) == 0 {
		return refusalEmpty
	}
	return ""
}

func outputConflicts(artifact []byte, present, oversized bool, finalMessage []byte) bool {
	return present && (oversized || len(artifact) == 0 || !bytes.Equal(artifact, finalMessage))
}

func validateEvaluationInput(input predicate.Input, raw predicate.Evidence, out io.Writer) error {
	if !input.Seal.Predicate.Equal(Reference()) {
		return fmt.Errorf("%w: codex predicate reference mismatch", task.ErrIdentityMismatch)
	}
	if err := task.ValidateProviderExitRecord(&input.Seal); err != nil {
		return err
	}
	if raw == nil {
		return errors.New("nil codex evidence")
	}
	if out == nil {
		return errors.New("nil codex answer sink")
	}
	return nil
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

func identityMatches(input predicate.Input, threadID string) bool {
	if !validThreadID(threadID) {
		return false
	}
	if input.ExpectedSession.Required {
		if !validThreadID(input.ExpectedSession.ID) || input.ExpectedSession.ID != threadID {
			return false
		}
	} else if input.ExpectedSession.ID != "" {
		return false
	}
	if input.RecordedSession != nil && (input.RecordedSession.Provider != Provider || input.RecordedSession.ConversationID != threadID) {
		return false
	}
	return true
}

func readOptionalOutput(raw predicate.Evidence) ([]byte, bool, bool, error) {
	named, ok := raw.(predicate.NamedEvidence)
	if !ok {
		return nil, false, false, errors.New("codex predicate requires named evidence")
	}
	var artifact []byte
	var oversized bool
	err := named.ReadNamed(OutputName, func(reader io.Reader) error {
		var readErr error
		artifact, oversized, readErr = readBounded(reader, maxEventLineBytes)
		return readErr
	})
	if errors.Is(err, predicate.ErrEvidenceAbsent) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, err
	}
	return artifact, true, oversized, nil
}

// readBounded retains one byte beyond the semantic bound while continuing to
// consume the supplied reader. Oversize is a predicate conflict; reader
// failures remain operational errors and are returned to the core.
func readBounded(reader io.Reader, maxBytes int) ([]byte, bool, error) {
	if reader == nil {
		return nil, false, errors.New("nil codex evidence reader")
	}
	data := make([]byte, 0, maxBytes+1)
	buffer := make([]byte, readBufferBytes)
	tooLarge := false
	noProgress := 0
	for {
		n, err := reader.Read(buffer)
		if n < 0 || n > len(buffer) {
			return nil, tooLarge, fmt.Errorf("reader returned invalid byte count %d", n)
		}
		if n > 0 {
			noProgress = 0
			data, tooLarge = retainBounded(data, buffer[:n], maxBytes, tooLarge)
		} else if err == nil {
			noProgress++
			if noProgress >= 100 {
				return nil, tooLarge, io.ErrNoProgress
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return data, tooLarge, nil
			}
			return nil, tooLarge, err
		}
	}
}

func retainBounded(data, chunk []byte, maxBytes int, tooLarge bool) ([]byte, bool) {
	if len(data) < maxBytes+1 {
		remaining := maxBytes + 1 - len(data)
		if len(chunk) > remaining {
			chunk = chunk[:remaining]
		}
		data = append(data, chunk...)
	}
	if len(data) > maxBytes {
		tooLarge = true
	}
	return data, tooLarge
}

func writeAnswer(out io.Writer, data []byte) error {
	if out == nil {
		return errors.New("nil codex answer sink")
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

func rejected(reason string) task.Interpretation {
	return task.Interpretation{Verdict: task.VerdictRejected, Refusal: reason}
}

func rejectedWithUsage(reason string, usage []task.UsageMetadata) task.Interpretation {
	result := rejected(reason)
	result.Usage = usage
	return result
}
