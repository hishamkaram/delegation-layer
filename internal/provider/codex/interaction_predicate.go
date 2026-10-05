package codex

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const interactionContract = "codex app-server stdio v1: one root thread and turn; exact session identity; final root agent message; completed root turn; zero exit; fatal native errors remain fatal, explicit transient retries require successful completion; duplicate JSON keys and incomplete streams reject; clarification never authorizes tools; steering acknowledgement is not compliance; interaction journal sealed alongside stdout and stderr\n"

type interactionInterpreter struct{ mode string }

func interactionReference(mode string) task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: "interactive-v1", SHA256: task.ComputeSHA256([]byte(interactionContract + mode + "\n"))}
}

func (v interactionInterpreter) Reference() task.PredicateRef { return interactionReference(v.mode) }

func (v interactionInterpreter) Evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if !input.Seal.Predicate.Equal(v.Reference()) {
		return task.Interpretation{}, task.ErrIdentityMismatch
	}
	if err := task.ValidateProviderExitRecord(&input.Seal); err != nil {
		return task.Interpretation{}, err
	}
	if raw == nil || out == nil {
		return task.Interpretation{}, task.ErrEvidenceFault
	}
	p := &appServerProtocol{expected: input.ExpectedSession, phase: 1, requests: map[string]json.RawMessage{}, steering: map[string]bool{}, replay: true}
	var semantic error
	readErr := raw.Read(predicate.Stdout, func(r io.Reader) error {
		var err error
		semantic, err = commonprovider.ReadJSONL(r, maxEventLineBytes, func(line []byte) error { _, e := p.Receive(line); return e })
		return err
	})
	if err := errors.Join(readErr, raw.Read(predicate.Stderr, commonprovider.DrainReader)); err != nil {
		return task.Interpretation{}, err
	}
	if refusal := sealRefusal(input.Seal); refusal != "" {
		return rejected(refusal), nil
	}
	if !interactionStreamComplete(semantic, p.Complete()) {
		return rejected(refusalIncomplete), nil
	}
	if !identityMatches(input, p.threadID) {
		return rejected(refusalIdentity), nil
	}
	if strings.TrimSpace(p.final) == "" {
		return rejected(refusalEmpty), nil
	}
	if err := writeAnswer(out, []byte(p.final)); err != nil {
		return task.Interpretation{}, err
	}
	return task.Interpretation{Verdict: task.VerdictCommitted, Session: &task.SessionIdentity{Provider: Provider, ConversationID: p.threadID}}, nil
}

func interactionStreamComplete(semantic, completion error) bool {
	return semantic == nil && completion == nil
}
