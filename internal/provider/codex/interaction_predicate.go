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

const strictInteractionContract = "codex app-server stdio v2: one root thread and turn; exact session identity; final root agent message selected by offset and streamed from sealed evidence; completed root turn; zero exit; fatal native errors remain fatal, explicit transient retries require successful completion; selected control duplicates reject while opaque tool payloads are discarded; clarification input is bounded and never authorizes tools; steering acknowledgement is not compliance; unknown lifecycle/control requests cannot imply completion or approval; interaction journal sealed alongside stdout and stderr\n"

const currentInteractionContract = "codex app-server stdio v3: one root thread and turn; exact session identity; final root agent message selected by offset and streamed from sealed evidence; completed root turn; zero exit; fatal native errors remain fatal, explicit transient retries require successful completion; selected control duplicates reject while opaque tool payloads are discarded; clarification input is bounded and never authorizes tools; steering acknowledgement is not compliance; a method with an id is a request: only the clarification request is processed and every other is denied by id even when it is named like a notification, a clarification request without an id is a fault, and an id that is not a string or number is a fault; unrecognized notifications without an id are ignored and cannot imply completion or approval; interaction journal sealed alongside stdout and stderr\n"

// interactionRevision names one frozen Codex interactive predicate. It is the
// only place that maps a predicate to its parsing behavior, so live capture,
// profile reconstruction, replay, and recovery cannot disagree.
type interactionRevision uint8

const (
	// interactionHistorical is interactive-v1: whole-line framing.
	interactionHistorical interactionRevision = iota
	// interactionStrict is interactive-v2: record capture; any unhandled
	// thread/, turn/, item/, or serverRequest/ notification is a fault.
	interactionStrict
	// interactionCurrent is interactive-v3: record capture; a method with an
	// id is a request, and unhandled notifications without one are ignored.
	interactionCurrent
)

var interactionRevisions = []interactionRevision{interactionHistorical, interactionStrict, interactionCurrent}

func (r interactionRevision) reference(mode string) task.PredicateRef {
	switch r {
	case interactionHistorical:
		return historicalInteractionReference(mode)
	case interactionStrict:
		return strictInteractionReference(mode)
	case interactionCurrent:
		return interactionReference(mode)
	}
	return task.PredicateRef{}
}

func (r interactionRevision) usesRecordCapture() bool { return r != interactionHistorical }

func (r interactionRevision) ignoresUnidentifiedNotifications() bool { return r == interactionCurrent }

// classifiesEnvelopesByID makes a method envelope with an id a request even
// when its name equals a notification the parser handles.
func (r interactionRevision) classifiesEnvelopesByID() bool { return r == interactionCurrent }

// recoversFrom returns the one earlier revision this revision may assess.
func (r interactionRevision) recoversFrom() (interactionRevision, bool) {
	switch r {
	case interactionStrict:
		return interactionHistorical, true
	case interactionCurrent:
		return interactionStrict, true
	case interactionHistorical:
		return 0, false
	}
	return 0, false
}

func interactionRevisionOf(reference task.PredicateRef, mode string) (interactionRevision, bool) {
	for _, revision := range interactionRevisions {
		if reference.Equal(revision.reference(mode)) {
			return revision, true
		}
	}
	return 0, false
}

type interactionInterpreter struct {
	mode     string
	revision interactionRevision
}

func interactionReference(mode string) task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: "interactive-v3", SHA256: task.ComputeSHA256([]byte(currentInteractionContract + mode + "\n"))}
}

func strictInteractionReference(mode string) task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: "interactive-v2", SHA256: task.ComputeSHA256([]byte(strictInteractionContract + mode + "\n"))}
}

func historicalInteractionReference(mode string) task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: mode, Version: "interactive-v1", SHA256: task.ComputeSHA256([]byte(interactionContract + mode + "\n"))}
}

func newInteractionInterpreter(mode string) predicate.Interpreter {
	return interactionInterpreter{mode: mode, revision: interactionCurrent}
}

func newStrictInteractionInterpreter(mode string) predicate.Interpreter {
	return interactionInterpreter{mode: mode, revision: interactionStrict}
}

func newHistoricalInteractionInterpreter(mode string) predicate.Interpreter {
	return interactionInterpreter{mode: mode, revision: interactionHistorical}
}

func (v interactionInterpreter) Reference() task.PredicateRef { return v.revision.reference(v.mode) }

func (v interactionInterpreter) Evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	if v.revision.usesRecordCapture() {
		return evaluateCurrentInteraction(v, input, raw, out)
	}
	return v.evaluateHistorical(input, raw, out)
}

func (v interactionInterpreter) evaluateHistorical(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
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
