package opencode

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// acpMessageID is the bounded identity of one decoded root messageId: the
// SHA-256 and byte length of its decoded string. Equivalent JSON escapes of
// one string have one identity; no ID bytes are retained.
type acpMessageID struct {
	length int64
	sum    [sha256.Size]byte
}

// acpMessageIDState classifies the messageId member of one root chunk.
type acpMessageIDState int

const (
	// acpMessageIDAbsent is a missing or null messageId.
	acpMessageIDAbsent acpMessageIDState = iota + 1
	// acpMessageIDMalformed is a duplicated or nonstring, nonnull messageId.
	acpMessageIDMalformed
	// acpMessageIDPresent is a string messageId, including the empty string.
	acpMessageIDPresent
)

// acpAnswerScope names the root text a replay streams to its sink.
type acpAnswerScope int

const (
	// acpScopeNone streams nothing; the facts-only replays use it.
	acpScopeNone acpAnswerScope = iota
	// acpScopeFinalRun streams the text of the final run only.
	acpScopeFinalRun
	// acpScopeWholeTurn streams every root text chunk of the prompt.
	acpScopeWholeTurn
)

// acpRunFacts are the decision-relevant run facts one replay derives from the
// root message chunks of the prompt. Runs counts maximal sequences of root
// chunks naming one messageId; last is the identity of the final run and
// lastNonblank whether that run had nonwhitespace text.
type acpRunFacts struct {
	runs         int64
	last         acpMessageID
	lastNonblank bool
	unavailable  bool
	malformed    bool
}

// acpRunTarget carries the first replay's final run into later replays.
type acpRunTarget struct {
	runs  int64
	final acpMessageID
	scope acpAnswerScope
}

// acpMessageRuns observes root message runs for one replay. It retains only
// fixed-size facts. repeated is known only once a target names the final
// run: the final messageId also named an earlier run. err is an operational
// evidence read failure, never a property of the native stream.
type acpMessageRuns struct {
	target   *acpRunTarget
	facts    acpRunFacts
	repeated bool
	err      error
}

// message observes one root agent_message_chunk. Its messageId is decoded
// only here, so selector metadata never becomes a reducer fault: the live
// protocol and acp-v1 never read it.
func (r *acpMessageRuns) message(update providerjson.Value) {
	id, state, err := decodeACPMessageID(update)
	if err != nil {
		if r.err == nil {
			r.err = err
		}
		return
	}
	switch state {
	case acpMessageIDAbsent:
		r.facts.unavailable = true
	case acpMessageIDMalformed:
		r.facts.malformed = true
	case acpMessageIDPresent:
		r.observe(id)
	}
}

// observe extends the current run or starts the next one.
func (r *acpMessageRuns) observe(id acpMessageID) {
	if r.facts.runs > 0 && id == r.facts.last {
		return
	}
	r.facts.runs++
	r.facts.last, r.facts.lastNonblank = id, false
	if r.target != nil && r.facts.runs < r.target.runs && id == r.target.final {
		r.repeated = true
	}
}

// text records decoded root text of the current run.
func (r *acpMessageRuns) text(p []byte) {
	if !r.facts.lastNonblank && len(bytes.TrimSpace(p)) > 0 {
		r.facts.lastNonblank = true
	}
}

// streams reports whether the current root text belongs to the selected
// answer.
func (r *acpMessageRuns) streams() bool {
	if r.target == nil {
		return false
	}
	switch r.target.scope {
	case acpScopeNone:
		return false
	case acpScopeFinalRun:
		return r.facts.runs == r.target.runs
	case acpScopeWholeTurn:
		return true
	default:
		return false
	}
}

// readFault returns the operational read failure of an optional observer.
func (r *acpMessageRuns) readFault() error {
	if r == nil {
		return nil
	}
	return r.err
}

// decodeACPMessageID streams the decoded messageId of update into SHA-256
// without a length cap, trimming, or interpretation. A syntax or duplicate
// field fault is malformed metadata; any other failure is an evidence read
// fault returned to the caller.
func decodeACPMessageID(update providerjson.Value) (acpMessageID, acpMessageIDState, error) {
	fields, err := update.Object("messageId")
	if err != nil {
		return acpMessageIDFault(err)
	}
	value, ok := fields["messageId"]
	if !ok {
		return acpMessageID{}, acpMessageIDAbsent, nil
	}
	if value.End-value.Start == int64(len("null")) {
		raw, rawErr := value.Raw(len("null"))
		if rawErr != nil {
			return acpMessageIDFault(rawErr)
		}
		if string(raw) == "null" {
			return acpMessageID{}, acpMessageIDAbsent, nil
		}
	}
	digest := sha256.New()
	counted := &acpCountingWriter{out: digest}
	if err = value.CopyString(counted); err != nil {
		return acpMessageIDFault(err)
	}
	id := acpMessageID{length: counted.length}
	digest.Sum(id.sum[:0])
	return id, acpMessageIDPresent, nil
}

func acpMessageIDFault(err error) (acpMessageID, acpMessageIDState, error) {
	var field *providerjson.FieldError
	if errors.As(err, &field) {
		return acpMessageID{}, acpMessageIDMalformed, nil
	}
	return acpMessageID{}, 0, fmt.Errorf("reading OpenCode ACP messageId: %w", err)
}

// acpCountingWriter counts the decoded bytes written through it.
type acpCountingWriter struct {
	out    io.Writer
	length int64
}

func (w *acpCountingWriter) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	w.length += int64(n)
	return n, err
}

// evaluateFinalRun is the acp-v2 interpretation, through three bounded
// replays of the canonical reducer. The first validates the turn and finds
// the final run; the second learns whether the final messageId also named an
// earlier run; the third streams the selected answer. Each later replay must
// reproduce the facts of the earlier ones.
func evaluateFinalRun(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	survey := &acpMessageRuns{}
	session, err := replayACP(input, raw, nil, survey)
	if err != nil {
		return task.Interpretation{}, err
	}
	if refusal := acpTurnRefusal(input, session); refusal != "" {
		return acpRejection(refusal, session), nil
	}
	if survey.facts.malformed {
		return acpRejection(refusalMalformed, session), nil
	}
	target := acpRunTarget{runs: survey.facts.runs, final: survey.facts.last, scope: acpScopeNone}
	check := &acpMessageRuns{target: &target}
	if err = replayFinalRun(input, raw, nil, session, check, survey.facts); err != nil {
		return task.Interpretation{}, err
	}
	scope, diagnostics := acpSelectAnswer(check.facts, check.repeated)
	nonblank := check.facts.lastNonblank
	if scope == acpScopeWholeTurn {
		nonblank = session.answer.nonblank
	}
	if !nonblank {
		return acpRejection(refusalEmpty, session, diagnostics...), nil
	}
	target.scope = scope
	streamed := &acpMessageRuns{target: &target}
	if err = replayFinalRun(input, raw, out, session, streamed, check.facts); err != nil {
		return task.Interpretation{}, err
	}
	if streamed.repeated != check.repeated {
		return task.Interpretation{}, fmt.Errorf("%w: OpenCode ACP replay was not deterministic", task.ErrEvidenceFault)
	}
	return acpCommitted(session, diagnostics...), nil
}

// replayFinalRun replays the evidence again and requires the same completed
// session, whole-turn answer facts, and run facts as the first replay.
func replayFinalRun(input predicate.Input, raw predicate.Evidence, sink io.Writer, first *acpSession, runs *acpMessageRuns, want acpRunFacts) error {
	replayed, err := replayACP(input, raw, sink, runs)
	if err != nil {
		return err
	}
	if replayed.completion() != nil || replayed.session != first.session || replayed.answer.seen != first.answer.seen ||
		replayed.answer.nonblank != first.answer.nonblank || runs.facts != want {
		return fmt.Errorf("%w: OpenCode ACP replay was not deterministic", task.ErrEvidenceFault)
	}
	return nil
}

// acpSelectAnswer selects the final run unless the message boundaries are
// unavailable or ambiguous; then every root text chunk is the answer and a
// diagnostic reports why.
func acpSelectAnswer(facts acpRunFacts, repeated bool) (acpAnswerScope, []task.OutputDiagnostic) {
	switch {
	case facts.unavailable:
		return acpScopeWholeTurn, []task.OutputDiagnostic{{
			Code:    "answer_message_boundary_unavailable",
			Message: "OpenCode did not identify every root answer message; the answer is all root text of the prompt.",
			Field:   "messageId",
		}}
	case repeated:
		return acpScopeWholeTurn, []task.OutputDiagnostic{{
			Code:    "answer_message_boundary_ambiguous",
			Message: "OpenCode's final answer message also appeared earlier in the prompt; the answer is all root text of the prompt.",
			Field:   "messageId",
		}}
	default:
		return acpScopeFinalRun, nil
	}
}
