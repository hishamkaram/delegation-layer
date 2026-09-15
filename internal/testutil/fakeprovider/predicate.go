package fakeprovider

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	maxFrameBytes = 64 * 1024
	stderrMarker  = "DELEGATE_FIXTURE_ERROR"
	readChunkSize = 32 * 1024
)

type fixtureV2 struct{}

// Predicate returns the finite provider's exact v2 interpreter.
func Predicate() predicate.Interpreter { return fixtureV2{} }

func (fixtureV2) Reference() task.PredicateRef {
	return task.PredicateRef{Adapter: Provider, Mode: Mode, Version: Version, SHA256: ContractDigest()}
}

func (v fixtureV2) Evaluate(input predicate.Input, raw predicate.Evidence, out io.Writer) (task.Interpretation, error) {
	ref := v.Reference()
	if !input.Seal.Predicate.Equal(ref) {
		return task.Interpretation{}, fmt.Errorf("%w: fixture v2 seal predicate mismatch", task.ErrIdentityMismatch)
	}
	if err := task.ValidateProviderExitRecord(&input.Seal); err != nil {
		return task.Interpretation{}, err
	}
	if raw == nil {
		return task.Interpretation{}, errors.New("nil fixture evidence")
	}
	if out == nil {
		return task.Interpretation{}, errors.New("nil fixture answer sink")
	}

	state := newEventState(input.Seal.TaskID, input.ExpectedSession, input.RecordedSession)
	stdoutErr := raw.Read(predicate.Stdout, func(reader io.Reader) error {
		return consumeStdout(reader, state, out)
	})
	stderrErr := raw.Read(predicate.Stderr, func(reader io.Reader) error {
		return consumeStderr(reader, state)
	})
	if err := errors.Join(stdoutErr, stderrErr, state.writeErr); err != nil {
		return task.Interpretation{}, err
	}

	refusal := state.refusal(&input.Seal)
	result := task.Interpretation{}
	if state.identity != nil {
		identity := *state.identity
		result.Session = &identity
	}
	if refusal != "" {
		result.Verdict = task.VerdictRejected
		result.Refusal = refusal
		return result, nil
	}
	result.Verdict = task.VerdictCommitted
	return result, nil
}

type wireEvent struct {
	Type      *string `json:"type,omitempty"`
	TaskID    *string `json:"task_id,omitempty"`
	SessionID *string `json:"session_id,omitempty"`
	Text      *string `json:"text,omitempty"`
	Success   *bool   `json:"success,omitempty"`
}

type event struct {
	typ       string
	taskID    string
	sessionID string
	text      string
	success   bool
}

func decodeEvent(frame []byte) (event, error) {
	if len(frame) == 0 || !utf8.Valid(frame) {
		return event{}, errors.New("invalid fixture frame bytes")
	}
	var wire wireEvent
	if err := task.DecodeStrict(frame, &wire); err != nil {
		return event{}, err
	}
	if wire.Type == nil || wire.TaskID == nil {
		return event{}, errors.New("fixture event is missing type or task_id")
	}
	result := event{typ: *wire.Type, taskID: *wire.TaskID}
	switch result.typ {
	case "session":
		return decodeSessionEvent(result, wire)
	case "answer_chunk":
		return decodeAnswerEvent(result, wire)
	case "result":
		return decodeResultEvent(result, wire)
	default:
		return event{}, fmt.Errorf("unsupported fixture event type %q", result.typ)
	}
}

func decodeSessionEvent(result event, wire wireEvent) (event, error) {
	if wire.SessionID == nil || wire.Text != nil || wire.Success != nil {
		return event{}, errors.New("invalid session event fields")
	}
	result.sessionID = *wire.SessionID
	return result, nil
}

func decodeAnswerEvent(result event, wire wireEvent) (event, error) {
	if wire.Text == nil || wire.SessionID != nil || wire.Success != nil {
		return event{}, errors.New("invalid answer_chunk event fields")
	}
	result.text = *wire.Text
	return result, nil
}

func decodeResultEvent(result event, wire wireEvent) (event, error) {
	if wire.SessionID == nil || wire.Success == nil || wire.Text != nil {
		return event{}, errors.New("invalid result event fields")
	}
	result.sessionID, result.success = *wire.SessionID, *wire.Success
	return result, nil
}

type semanticFault struct{ reason string }

type eventState struct {
	taskID      string
	expected    task.SessionExpectation
	recorded    *task.SessionIdentity
	identity    *task.SessionIdentity
	sessionID   string
	sessionSeen bool
	resultSeen  bool
	answerNonWS bool
	semantic    *semanticFault
	stderrError bool
	writeErr    error
	write       func(string) error
}

func newEventState(taskID string, expected task.SessionExpectation, recorded *task.SessionIdentity) *eventState {
	return &eventState{taskID: taskID, expected: expected, recorded: recorded}
}

func (s *eventState) latch(reason string) {
	if s.semantic == nil {
		s.semantic = &semanticFault{reason: reason}
	}
}

func (s *eventState) apply(e event) bool {
	if s.semantic != nil {
		return false
	}
	if e.taskID != s.taskID {
		s.latch("task_identity_mismatch")
		return false
	}
	switch e.typ {
	case "session":
		return s.applySession(e)
	case "answer_chunk":
		return s.applyAnswer(e)
	case "result":
		return s.applyResult(e)
	default:
		s.latch("malformed_output")
		return false
	}
}

func (s *eventState) applySession(e event) bool {
	if !validSessionID(e.sessionID) || !s.sessionMatches(e.sessionID) {
		s.latch("session_identity_mismatch")
		return false
	}
	if s.sessionSeen && e.sessionID != s.sessionID {
		s.latch("session_identity_mismatch")
		return false
	}
	if s.resultSeen {
		s.latch("event_after_result")
		return false
	}
	if s.sessionSeen {
		s.latch("duplicate_session")
		return false
	}
	s.sessionSeen, s.sessionID = true, e.sessionID
	s.identity = &task.SessionIdentity{Provider: Provider, ConversationID: e.sessionID}
	return true
}

func (s *eventState) applyAnswer(e event) bool {
	if s.resultSeen {
		s.latch("event_after_result")
		return false
	}
	if strings.IndexFunc(e.text, func(r rune) bool { return !unicode.IsSpace(r) }) >= 0 {
		s.answerNonWS = true
	}
	if s.write != nil && s.writeErr == nil {
		s.writeErr = s.write(e.text)
	}
	return false
}

func (s *eventState) applyResult(e event) bool {
	if !validSessionID(e.sessionID) || !s.sessionMatches(e.sessionID) || !s.sessionSeen || e.sessionID != s.sessionID {
		s.latch("session_identity_mismatch")
		return false
	}
	if s.resultSeen {
		s.latch("event_after_result")
		return false
	}
	s.resultSeen = true
	if !e.success {
		s.latch("provider_failure")
	}
	return false
}

func (s *eventState) sessionMatches(id string) bool {
	if s.expected.Required && s.expected.ID != id {
		return false
	}
	if s.recorded != nil && (s.recorded.Provider != Provider || s.recorded.ConversationID != id) {
		return false
	}
	return true
}

func (s *eventState) refusal(seal *task.ProviderExitRecord) string {
	switch {
	case seal.InvocationState == task.InvocationStartFailed:
		return "start_failed: " + seal.Error
	case seal.Error != "":
		return "capture_error: " + seal.Error
	case seal.ExitCode != 0:
		return fmt.Sprintf("provider_exit: %d", seal.ExitCode)
	case s.stderrError:
		return "stderr_error"
	case s.semantic != nil:
		return s.semantic.reason
	case !s.sessionSeen:
		return "missing_session"
	case !s.resultSeen:
		return "missing_result"
	case !s.answerNonWS:
		return "empty_answer"
	default:
		return ""
	}
}

func validSessionID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	if !isASCIIAlphaNumeric(id[0]) {
		return false
	}
	for i := 1; i < len(id); i++ {
		c := id[i]
		if !isSessionIDPart(c) {
			return false
		}
	}
	return true
}

func isASCIIAlphaNumeric(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

func isSessionIDPart(c byte) bool {
	return isASCIIAlphaNumeric(c) || c == '.' || c == '_' || c == ':' || c == '-'
}

type frameScanner struct {
	pending []byte
	stopped bool
	onFrame func([]byte)
	isFault func() bool
}

func (s *frameScanner) feed(data []byte) {
	if s.stopped {
		return
	}
	for len(data) > 0 && !s.stopped {
		lf := bytes.IndexByte(data, '\n')
		if lf < 0 {
			if len(s.pending)+len(data) > maxFrameBytes {
				s.onFrame(nil)
				s.stopped = true
				return
			}
			s.pending = append(s.pending, data...)
			return
		}
		frameLen := len(s.pending) + lf
		if frameLen == 0 || frameLen > maxFrameBytes {
			s.onFrame(nil)
			s.stopped = true
			return
		}
		frame := make([]byte, 0, frameLen)
		frame = append(frame, s.pending...)
		frame = append(frame, data[:lf]...)
		s.pending = s.pending[:0]
		s.onFrame(frame)
		if s.isFault() {
			s.stopped = true
			return
		}
		data = data[lf+1:]
	}
}

func (s *frameScanner) finish() {
	if s.stopped {
		return
	}
	if len(s.pending) > 0 {
		s.onFrame(nil)
		s.pending = nil
	}
	s.stopped = true
}

func consumeStdout(reader io.Reader, state *eventState, out io.Writer) error {
	state.write = func(text string) error {
		n, err := io.WriteString(out, text)
		if err == nil && n != len(text) {
			return io.ErrShortWrite
		}
		return err
	}
	scanner := frameScanner{onFrame: func(frame []byte) {
		if frame == nil {
			state.latch("malformed_output")
			return
		}
		e, err := decodeEvent(frame)
		if err != nil {
			state.latch("malformed_output")
			return
		}
		state.apply(e)
	}, isFault: func() bool { return state.semantic != nil }}
	return consume(reader, func(chunk []byte) { scanner.feed(chunk) }, scanner.finish)
}

func consume(reader io.Reader, onChunk func([]byte), finish func()) error {
	if reader == nil {
		return errors.New("nil fixture stream reader")
	}
	buf := make([]byte, readChunkSize)
	noProgress := 0
	for {
		n, err := reader.Read(buf)
		if n < 0 || n > len(buf) {
			return fmt.Errorf("fixture reader returned invalid byte count %d", n)
		}
		if n > 0 {
			noProgress = 0
			onChunk(buf[:n])
		} else if err == nil {
			noProgress++
			if noProgress == 100 {
				return io.ErrNoProgress
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				finish()
				return nil
			}
			return err
		}
	}
}

func consumeStderr(reader io.Reader, state *eventState) error {
	scanner := &markerScanner{}
	return consume(reader, scanner.feed, func() { state.stderrError = scanner.found })
}

type markerScanner struct {
	tail  []byte
	found bool
}

func (s *markerScanner) feed(chunk []byte) {
	if s.found {
		return
	}
	combined := make([]byte, 0, len(s.tail)+len(chunk))
	combined = append(combined, s.tail...)
	combined = append(combined, chunk...)
	if bytes.Contains(combined, []byte(stderrMarker)) {
		s.found = true
		return
	}
	keep := len(stderrMarker) - 1
	if len(combined) > keep {
		combined = combined[len(combined)-keep:]
	}
	s.tail = append(s.tail[:0], combined...)
}

var (
	_ predicate.Interpreter      = fixtureV2{}
	_ execution.IdentityObserver = (*identityObserver)(nil)
)
