package claude

import (
	"errors"
	"io"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const historicalInteractionContract = "Claude stream-json host clarification v1: one initial user input; exact native session; repeated same-session initialization permitted before completion; AskUserQuestion answers only; never authorize other tools; native result plus idle and settled tracked agents; fresh root result after background completion; earlier provider errors remain fatal; latest successful nonempty root result; zero exit; malformed, incomplete or conflicting evidence rejects; control journal sealed with raw streams\n"

const previousInteractionContract = "Claude stream-json host clarification v2: one initial user input; exact native session; repeated same-session initialization permitted before completion; AskUserQuestion answers only; never authorize other tools; native result plus idle and settled tracked agents; fresh root result after background completion; earlier provider errors remain fatal; latest successful nonempty root result; offset-streamed sealed records without a native event-size ceiling; malformed, incomplete or conflicting evidence rejects; control journal sealed with raw streams\n"

const interactionContract = "Claude stream-json host clarification v3: one initial user input; exact native session; repeated same-session initialization permitted before completion; AskUserQuestion answers only; never authorize other tools; native result plus idle and settled tracked agents; fresh root result after background completion; earlier provider errors remain fatal; latest successful nonempty root result; offset-streamed sealed records without a native event-size ceiling; stable runtime probe requires only --print; malformed, incomplete or conflicting evidence rejects; control journal sealed with raw streams\n"

type interactionInterpreter struct {
	mode       string
	historical bool
	reference  *task.PredicateRef
}

func interactionReference(mode string) task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: "interactive-v3", SHA256: task.ComputeSHA256([]byte(interactionContract + mode + "\n"))}
}

func previousInteractionReference(mode string) task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: "interactive-v2", SHA256: task.ComputeSHA256([]byte(previousInteractionContract + mode + "\n"))}
}

func historicalInteractionReference(mode string) task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: "interactive-v1", SHA256: task.ComputeSHA256([]byte(historicalInteractionContract + mode + "\n"))}
}

func newPreviousInteractionInterpreter(mode string) predicate.Interpreter {
	reference := previousInteractionReference(mode)
	return interactionInterpreter{mode: mode, reference: &reference}
}

func (v interactionInterpreter) Reference() task.PredicateRef {
	if v.reference != nil {
		return *v.reference
	}
	if v.historical {
		return historicalInteractionReference(v.mode)
	}
	return interactionReference(v.mode)
}

func (v interactionInterpreter) Evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	return v.evaluateAgainst(input, raw, out, v.Reference())
}

func (v interactionInterpreter) evaluateAgainst(input predicate.Input, raw predicate.Evidence, out io.Writer, reference task.PredicateRef) (task.Interpretation, error) {
	ref := v.Reference()
	if err := validateEvaluationInput(interpreter{mode: v.mode, streamReference: &ref}, input, raw, out, reference); err != nil {
		return task.Interpretation{}, err
	}
	p := newSealedClaudeInteraction(v, input)
	semantic, readErr := v.readInteractionStdout(raw, p)
	if err := errors.Join(readErr, raw.Read(predicate.Stderr, commonprovider.DrainReader)); err != nil {
		return task.Interpretation{}, err
	}
	p.state.resultSeen = p.resultSeen
	if !interactionStreamComplete(semantic, p.Complete()) {
		return v.incompleteInteractionResult(p), nil
	}
	if refusal := refusalFor(input, p.state); refusal != "" {
		return v.refusedInteractionResult(refusal, p), nil
	}
	if err := v.writeInteractionAnswer(raw, p, out); err != nil {
		return task.Interpretation{}, err
	}
	return v.committedInteractionResult(p), nil
}

func newSealedClaudeInteraction(v interactionInterpreter, input predicate.Input) *claudeInteraction {
	p := &claudeInteraction{state: eventState{mode: v.mode, current: !v.historical}, turnID: "sealed", pending: map[string]claudePendingQuestion{}, agents: map[string]bool{}}
	if input.RecordedSession != nil {
		p.expected = input.RecordedSession.ConversationID
	} else if input.ExpectedSession.Required {
		p.expected = input.ExpectedSession.ID
	}
	return p
}

func (v interactionInterpreter) readInteractionStdout(raw predicate.Evidence, p *claudeInteraction) (error, error) {
	if v.historical {
		return v.readHistoricalInteractionStdout(raw, p)
	}
	return v.readCurrentInteractionStdout(raw, p)
}

func (v interactionInterpreter) readHistoricalInteractionStdout(raw predicate.Evidence, p *claudeInteraction) (error, error) {
	var semantic error
	readErr := raw.Read(predicate.Stdout, func(reader io.Reader) error {
		var parserErr error
		semantic, parserErr = commonprovider.ReadJSONL(reader, maxEventLineBytes, func(line []byte) error {
			event, err := decodeEvent(line)
			if err != nil {
				return err
			}
			if event.typeName == "system" && p.expected == "" {
				p.expected, _, _ = readOptionalString(event.fields, "session_id")
			}
			_, err = p.Receive(line)
			return err
		})
		return parserErr
	})
	return semantic, readErr
}

func (v interactionInterpreter) readCurrentInteractionStdout(raw predicate.Evidence, p *claudeInteraction) (error, error) {
	var semantic error
	readErr := raw.Read(predicate.Stdout, func(reader io.Reader) error {
		var parserErr error
		semantic, parserErr = providerjson.ReadRecords(reader, func(value providerjson.Value, _ uint64) error {
			if p.expected == "" {
				if event, err := decodeClaudeCurrentEvent(value); err == nil && event.typeName == "system" {
					if session, sessionErr := claudeCurrentRequiredString(event.fields, "session_id", 128); sessionErr == nil {
						p.expected = session
					}
				}
			}
			_, err := p.ReceiveRecord(value)
			return err
		})
		return parserErr
	})
	return semantic, readErr
}

func (v interactionInterpreter) writeInteractionAnswer(raw predicate.Evidence, p *claudeInteraction, out io.Writer) error {
	if !v.historical {
		return raw.Read(predicate.Stdout, func(reader io.Reader) error {
			return copyClaudeSpans(reader, p.state.result.currentAnswer.spans, out)
		})
	}
	return writeAnswer(out, []byte(p.state.result.answer))
}

func (v interactionInterpreter) incompleteInteractionResult(p *claudeInteraction) task.Interpretation {
	result := rejectedWithUsage(refusalIncomplete, nil)
	if !v.historical {
		result.Diagnostics = append(result.Diagnostics, claudeCurrentDiagnostic(refusalIncomplete))
		result.Diagnostics = append(result.Diagnostics, claudeCurrentUsageWarning(p.state)...)
	}
	return result
}

func (v interactionInterpreter) refusedInteractionResult(refusal string, p *claudeInteraction) task.Interpretation {
	result := rejectedWithUsage(refusal, claudeCurrentUsageMetadata(p.state, task.UsageReliabilityUnreliable))
	if !v.historical {
		result.Diagnostics = append(result.Diagnostics, claudeCurrentDiagnostic(refusal))
		result.Diagnostics = append(result.Diagnostics, claudeCurrentUsageWarning(p.state)...)
	}
	return result
}

func (v interactionInterpreter) committedInteractionResult(p *claudeInteraction) task.Interpretation {
	result := task.Interpretation{Verdict: task.VerdictCommitted, Session: &task.SessionIdentity{Provider: Provider, ConversationID: p.expected}, Usage: claudeCurrentUsageMetadata(p.state, task.UsageReliabilityReported)}
	if !v.historical {
		result.Diagnostics = claudeCurrentUsageWarning(p.state)
	}
	return result
}

func (v interactionInterpreter) Target() task.PredicateRef { return v.Reference() }

// CanRecover permits the corrected interactive parser to reassess only the
// immediately preceding same-wire interactive revision for this mode.
func (v interactionInterpreter) CanRecover(source task.PredicateRef) bool {
	if v.historical || v.reference != nil {
		return false
	}
	return source.Equal(historicalInteractionReference(v.mode)) || source.Equal(previousInteractionReference(v.mode))
}

func (v interactionInterpreter) Recover(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if !v.CanRecover(input.Seal.Predicate) {
		return task.Interpretation{}, task.ErrIncompatiblePredicate
	}
	return v.evaluateAgainst(input, raw, out, input.Seal.Predicate)
}

func interactionStreamComplete(semantic, completion error) bool {
	return semantic == nil && completion == nil
}
