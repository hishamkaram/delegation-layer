package claude

import (
	"errors"
	"io"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const interactionContract = "Claude stream-json host clarification v1: one initial user input; exact native session; repeated same-session initialization permitted before completion; AskUserQuestion answers only; never authorize other tools; native result plus idle and settled tracked agents; fresh root result after background completion; earlier provider errors remain fatal; latest successful nonempty root result; zero exit; malformed, incomplete or conflicting evidence rejects; control journal sealed with raw streams\n"

type interactionInterpreter struct{ mode string }

func interactionReference(mode string) task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: "interactive-v1", SHA256: task.ComputeSHA256([]byte(interactionContract + mode + "\n"))}
}

func (v interactionInterpreter) Reference() task.PredicateRef { return interactionReference(v.mode) }

func (v interactionInterpreter) Evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	ref := v.Reference()
	if err := validateEvaluationInput(interpreter{mode: v.mode, streamReference: &ref}, input, raw, out); err != nil {
		return task.Interpretation{}, err
	}
	p := &claudeInteraction{state: eventState{mode: v.mode}, turnID: "sealed", pending: map[string]claudePendingQuestion{}, agents: map[string]bool{}}
	if input.RecordedSession != nil {
		p.expected = input.RecordedSession.ConversationID
	} else if input.ExpectedSession.Required {
		p.expected = input.ExpectedSession.ID
	}
	var semantic error
	readErr := raw.Read(predicate.Stdout, func(r io.Reader) error {
		var err error
		semantic, err = commonprovider.ReadJSONL(r, maxEventLineBytes, func(line []byte) error {
			event, e := decodeEvent(line)
			if e != nil {
				return e
			}
			if event.typeName == "system" && p.expected == "" {
				p.expected, _, _ = readOptionalString(event.fields, "session_id")
			}
			_, e = p.Receive(line)
			return e
		})
		return err
	})
	if err := errors.Join(readErr, raw.Read(predicate.Stderr, commonprovider.DrainReader)); err != nil {
		return task.Interpretation{}, err
	}
	p.state.resultSeen = p.resultSeen
	if !interactionStreamComplete(semantic, p.Complete()) {
		return rejectedWithUsage(refusalIncomplete, nil), nil
	}
	if refusal := refusalFor(input, p.state); refusal != "" {
		return rejectedWithUsage(refusal, usageFromResult(p.state, task.UsageReliabilityUnreliable)), nil
	}
	if err := writeAnswer(out, []byte(p.state.result.answer)); err != nil {
		return task.Interpretation{}, err
	}
	return task.Interpretation{Verdict: task.VerdictCommitted, Session: &task.SessionIdentity{Provider: Provider, ConversationID: p.expected}, Usage: usageFromResult(p.state, task.UsageReliabilityReported)}, nil
}

func interactionStreamComplete(semantic, completion error) bool {
	return semantic == nil && completion == nil
}
