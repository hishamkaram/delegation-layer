package contributorprovider

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	refusalMalformed       = "malformed-envelope"
	refusalIdentity        = "identity-mismatch"
	refusalEmptyOutput     = "empty-output"
	refusalOutputConflict  = "output-conflict"
	refusalProviderFailure = "provider-failed"
)

type interpreter struct{}

// NewInterpreter returns the immutable contributor-proof envelope predicate.
func NewInterpreter() predicate.Interpreter { return interpreter{} }

// Predicate is an alias convenient for deterministic fixture tests.
func Predicate() predicate.Interpreter { return NewInterpreter() }

func (interpreter) Reference() task.PredicateRef { return Reference() }

func (interpreter) Evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if err := validateEvaluationInput(input, raw, out); err != nil {
		return task.Interpretation{}, err
	}
	envelope, malformed, evidenceErr := readEnvelope(raw)
	if evidenceErr != nil {
		return task.Interpretation{}, evidenceErr
	}
	if refusal := sealRefusal(input.Seal); refusal != "" {
		return rejected(refusal), nil
	}
	if malformed {
		return rejected(refusalMalformed), nil
	}
	return interpretEnvelope(input, envelope, raw, out)
}

func validateEvaluationInput(input predicate.Input, raw predicate.Evidence, out io.Writer) error {
	if !input.Seal.Predicate.Equal(Reference()) {
		return fmt.Errorf("%w: contributor-proof predicate reference mismatch", task.ErrIdentityMismatch)
	}
	if err := task.ValidateProviderExitRecord(&input.Seal); err != nil {
		return err
	}
	if raw == nil {
		return errors.New("nil contributor-proof evidence")
	}
	if out == nil {
		return errors.New("nil contributor-proof answer sink")
	}
	return nil
}

func sealRefusal(seal task.ProviderExitRecord) string {
	if seal.InvocationState == task.InvocationStartFailed {
		return refusalProviderFailure + ": start_failed: " + seal.Error
	}
	if seal.Error != "" {
		return refusalProviderFailure + ": seal_error: " + seal.Error
	}
	if seal.ExitCode != 0 {
		return fmt.Sprintf("%s: exit_code=%d", refusalProviderFailure, seal.ExitCode)
	}
	return ""
}

func interpretEnvelope(input predicate.Input, envelope Envelope, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if !identityMatches(input, envelope) {
		return rejected(refusalIdentity), nil
	}
	if envelope.Status == "rejected" {
		if strings.TrimSpace(envelope.Answer) == "" {
			return rejected(refusalMalformed), nil
		}
		return rejected("provider-rejected: " + envelope.Answer), nil
	}
	if strings.TrimSpace(envelope.Answer) == "" {
		return rejected("empty-answer"), nil
	}

	named, ok := raw.(predicate.NamedEvidence)
	if !ok {
		return task.Interpretation{}, errors.New("contributor-proof requires named evidence")
	}
	artifact, present, err := readOptionalArtifact(named)
	if err != nil {
		return task.Interpretation{}, err
	}
	if present {
		if len(artifact) == 0 {
			return rejected(refusalEmptyOutput), nil
		}
		if !equalAnswer(artifact, envelope.Answer) {
			return rejected(refusalOutputConflict), nil
		}
		if err = writeAnswer(out, artifact); err != nil {
			return task.Interpretation{}, err
		}
	} else if err = writeAnswer(out, []byte(envelope.Answer)); err != nil {
		return task.Interpretation{}, err
	}

	session := task.SessionIdentity{Provider: Provider, ConversationID: envelope.SessionID}
	return task.Interpretation{Verdict: task.VerdictCommitted, Session: &session}, nil
}

func readEnvelope(raw predicate.Evidence) (Envelope, bool, error) {
	var envelope Envelope
	var malformed bool
	stdoutErr := raw.Read(predicate.Stdout, func(reader io.Reader) error {
		data, err := task.ReadBounded(reader, MaxEnvelopeBytes)
		if errors.Is(err, task.ErrControlRecordTooBig) {
			malformed = true
			return nil
		}
		if err != nil {
			return err
		}
		if decodeEnvelope(data, &envelope) != nil {
			malformed = true
		}
		return nil
	})
	stderrErr := raw.Read(predicate.Stderr, func(reader io.Reader) error {
		_, err := io.Copy(io.Discard, reader)
		return err
	})
	return envelope, malformed, errors.Join(stdoutErr, stderrErr)
}

func decodeEnvelope(data []byte, envelope *Envelope) error {
	// encoding/json replaces invalid UTF-8 inside strings. Validate original
	// evidence first so malformed provider bytes cannot become a valid answer.
	if !utf8.Valid(data) {
		return errors.New("invalid contributor-proof UTF-8 envelope")
	}
	if err := task.DecodeStrict(data, envelope); err != nil {
		return err
	}
	if envelope.Protocol != Protocol {
		return errors.New("invalid contributor-proof protocol envelope")
	}
	if err := task.ValidateTaskID(envelope.TaskID); err != nil {
		return err
	}
	if !validSessionID(envelope.SessionID) {
		return errors.New("invalid contributor-proof session ID")
	}
	switch envelope.Status {
	case "complete", "rejected":
		return nil
	default:
		return fmt.Errorf("invalid contributor-proof status %q", envelope.Status)
	}
}

func identityMatches(input predicate.Input, envelope Envelope) bool {
	if envelope.TaskID != input.Seal.TaskID {
		return false
	}
	expected := "session-" + input.Seal.TaskID
	if input.ExpectedSession.Required {
		expected = input.ExpectedSession.ID
	}
	if envelope.SessionID != expected {
		return false
	}
	if input.RecordedSession != nil && (input.RecordedSession.Provider != Provider || input.RecordedSession.ConversationID != envelope.SessionID) {
		return false
	}
	return true
}

func readOptionalArtifact(named predicate.NamedEvidence) ([]byte, bool, error) {
	var artifact []byte
	err := named.ReadNamed(OutputName, func(reader io.Reader) error {
		var err error
		// One byte beyond the envelope bound is enough to prove disagreement
		// with any admissible answer. Size overflow is semantic conflict, not
		// an evidence-reader failure; actual I/O errors still propagate.
		artifact, err = io.ReadAll(io.LimitReader(reader, MaxEnvelopeBytes+1))
		return err
	})
	if errors.Is(err, predicate.ErrEvidenceAbsent) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return artifact, true, nil
}

func writeAnswer(out io.Writer, answer []byte) error {
	n, err := out.Write(answer)
	if err == nil && n != len(answer) {
		return io.ErrShortWrite
	}
	return err
}

func equalAnswer(artifact []byte, answer string) bool {
	return string(artifact) == answer
}

func rejected(reason string) task.Interpretation {
	return task.Interpretation{Verdict: task.VerdictRejected, Refusal: reason}
}

func validSessionID(id string) bool {
	const prefix = "session-"
	if !strings.HasPrefix(id, prefix) {
		return false
	}
	return task.ValidateTaskID(strings.TrimPrefix(id, prefix)) == nil
}
