package antigravity

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	refusalInvalidOutput    = "invalid-output"
	refusalIdentityMismatch = "identity-mismatch"
	refusalIncompleteOutput = "incomplete-output"
	refusalProviderFailed   = "provider-failed"
)

type printInterpreter struct {
	mode                        string
	classifyErrorBeforeIdentity bool
}

// NewPrintInterpreter returns the immutable agy print-envelope interpreter.
func NewPrintInterpreter() predicate.Interpreter { return printInterpreter{mode: Mode} }

// NewCurrentPrintInterpreter classifies known ERROR envelopes before requiring
// a conversation UUID. The legacy constructor remains available for replay.
func NewCurrentPrintInterpreter() predicate.Interpreter {
	return NewCurrentPrintInterpreterForMode(Mode)
}

// NewCurrentPrintInterpreterForMode binds the current envelope rules to the
// requested containment mode so a read-only result cannot satisfy a write
// profile and vice versa.
func NewCurrentPrintInterpreterForMode(mode string) predicate.Interpreter {
	return printInterpreter{mode: mode, classifyErrorBeforeIdentity: true}
}

func (v printInterpreter) Reference() task.PredicateRef {
	mode := v.mode
	if mode == "" {
		mode = Mode
	}
	if v.classifyErrorBeforeIdentity {
		return task.PredicateRef{Adapter: Provider, Mode: mode, Version: "1.2.2-auth2", SHA256: task.ComputeSHA256([]byte(currentPrintContract))}
	}
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: Version, SHA256: ContractDigest()}
}

func (v printInterpreter) Evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if !input.Seal.Predicate.Equal(v.Reference()) {
		return task.Interpretation{}, fmt.Errorf("%w: antigravity predicate reference mismatch", task.ErrIdentityMismatch)
	}
	if err := task.ValidateProviderExitRecord(&input.Seal); err != nil {
		return task.Interpretation{}, err
	}
	if raw == nil {
		return task.Interpretation{}, errors.New("nil antigravity evidence")
	}
	if out == nil {
		return task.Interpretation{}, errors.New("nil antigravity answer sink")
	}

	var stdout stdoutResult
	stdoutReadErr := raw.Read(predicate.Stdout, func(reader io.Reader) error {
		stdout = parseStdout(reader, out)
		return stdout.operation
	})
	var timeout bool
	stderrReadErr := raw.Read(predicate.Stderr, func(reader io.Reader) error {
		var err error
		timeout, err = scanTimeout(reader)
		return err
	})
	if err := errors.Join(stdoutReadErr, stderrReadErr); err != nil {
		return task.Interpretation{}, err
	}
	return interpretEnvelope(input, stdout, timeout, v.classifyErrorBeforeIdentity), nil
}

func interpretEnvelope(input predicate.Input, stdout stdoutResult, timeout, classifyErrorBeforeIdentity bool) task.Interpretation {
	if input.Seal.InvocationState == task.InvocationStartFailed {
		return rejectedWithUsage(refusalProviderFailed+": start_failed", usageFromEnvelope(stdout.envelope, task.UsageReliabilityUnreliable))
	}
	if input.Seal.Error != "" {
		return rejectedWithUsage(refusalProviderFailed+": seal_error", unreliableUsage(stdout.envelope))
	}
	if stdout.semantic != nil {
		return rejectedWithUsage(refusalInvalidOutput, usageFromEnvelope(stdout.envelope, task.UsageReliabilityUnreliable))
	}
	if !knownStatus(stdout.envelope.status) {
		return rejectedWithUsage(refusalInvalidOutput, usageFromEnvelope(stdout.envelope, task.UsageReliabilityUnreliable))
	}
	if classifyErrorBeforeIdentity && stdout.envelope.status == StatusError {
		return rejectedWithUsage(providerFailureRefusal(stdout.envelope), unreliableUsage(stdout.envelope))
	}
	if err := validateConversation(stdout.envelope.conversationID, input.ExpectedSession, input.RecordedSession); err != nil {
		return rejectedWithUsage(refusalIdentityMismatch, usageFromEnvelope(stdout.envelope, task.UsageReliabilityUnreliable))
	}

	resultEnvelope := stdout.envelope
	unreliable := unreliableUsage(resultEnvelope)

	if resultEnvelope.status == StatusError {
		return rejectedWithUsage(providerFailureRefusal(resultEnvelope), unreliable)
	}
	if resultEnvelope.errorSeen {
		return rejectedWithUsage(refusalInvalidOutput, unreliable)
	}
	if input.Seal.ExitCode != 0 {
		return rejectedWithUsage(fmt.Sprintf("%s: exit_code=%d", refusalProviderFailed, input.Seal.ExitCode), unreliable)
	}
	if timeout || !resultEnvelope.responseNonWS {
		return rejectedWithUsage(refusalIncompleteOutput, unreliable)
	}

	identity := &task.SessionIdentity{Provider: Provider, ConversationID: resultEnvelope.conversationID}
	result := task.Interpretation{Verdict: task.VerdictCommitted, Session: identity, Usage: usageFromEnvelope(resultEnvelope, task.UsageReliabilityReported)}
	return result
}

func rejected(reason string) task.Interpretation {
	return task.Interpretation{Verdict: task.VerdictRejected, Refusal: reason}
}

func rejectedWithUsage(reason string, usage []task.UsageMetadata) task.Interpretation {
	result := rejected(reason)
	result.Usage = usage
	return result
}

func providerFailureRefusal(envelope envelope) string {
	if !envelope.errorSeen || envelope.errorText == "" {
		return refusalProviderFailed + ": status=" + envelope.status
	}
	return refusalProviderFailed + ": status=" + envelope.status + ": " + envelope.errorText
}

func validateConversation(id string, expected task.SessionExpectation, recorded *task.SessionIdentity) error {
	if !validConversationID(id) {
		return errors.New("invalid antigravity conversation ID")
	}
	if expected.Required {
		if !validConversationID(expected.ID) || expected.ID != id {
			return errors.New("expected antigravity conversation ID mismatch")
		}
	} else if expected.ID != "" {
		return errors.New("unexpected antigravity continuation ID")
	}
	if recorded != nil && (recorded.Provider != Provider || recorded.ConversationID != id) {
		return errors.New("recorded antigravity conversation ID mismatch")
	}
	return nil
}

func validConversationID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if id[i] != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", rune(id[i])) {
			return false
		}
	}
	return true
}

func unreliableUsage(envelope envelope) []task.UsageMetadata {
	return usageFromEnvelope(envelope, task.UsageReliabilityUnreliable)
}

func usageFromEnvelope(envelope envelope, reliability string) []task.UsageMetadata {
	if envelope.usage == nil && envelope.durationSeconds == nil && envelope.numTurns == nil {
		return nil
	}
	result := task.UsageMetadata{
		Scope:           task.UsageScopeConversationCumulative,
		Source:          task.UsageSourceProviderEnvelope,
		Reliability:     reliability,
		DurationSeconds: envelope.durationSeconds,
		NumTurns:        envelope.numTurns,
	}
	if envelope.usage != nil {
		result.InputTokens = envelope.usage.InputTokens
		result.OutputTokens = envelope.usage.OutputTokens
		result.ThinkingTokens = envelope.usage.ThinkingTokens
		result.TotalTokens = envelope.usage.TotalTokens
		result.CacheReadTokens = envelope.usage.CacheReadTokens
	}
	return []task.UsageMetadata{result}
}
