package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/providerjson"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type recordProtocol interface {
	execution.InteractionProtocol
	execution.CurrentRecordInteractionProtocol
}

// liveSession drives a protocol exactly as the execution layer does when the
// protocol selects record capture: one callback-scoped record per stdout line.
type liveSession struct {
	protocol recordProtocol
	outbound [][]byte
	events   []task.InteractionEvent
	done     bool
}

func (s *liveSession) feed(line string) (execution.InteractionUpdate, error) {
	update, err := s.protocol.ReceiveRecord(providerjson.Value{Reader: strings.NewReader(line), End: int64(len(line))})
	if err != nil {
		return update, err
	}
	s.outbound = append(s.outbound, update.Outbound...)
	s.events = append(s.events, update.Events...)
	s.done = s.done || update.Done
	return update, nil
}

func (s *liveSession) feedAll(t *testing.T, lines []string) {
	t.Helper()
	for _, line := range lines {
		if s.done {
			t.Fatalf("input closed before %s", line)
		}
		if _, err := s.feed(line); err != nil {
			t.Fatalf("live capture rejected %s: %v", line, err)
		}
	}
}

func interactiveRequest(t *testing.T, mode string) (task.TaskRecord, time.Time, commonprovider.PreparedProfile) {
	t.Helper()
	request, now := profileReconstructionFixture(t)
	request.Mode = mode
	request.RequestedConfig.Permission = mode
	request.RequestedConfig.Interactive = true
	candidate, err := PrepareCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	return request, now, finalizeProfile(t, candidate, now)
}

func startLiveSession(t *testing.T, profile commonprovider.PreparedProfile, expected task.SessionExpectation, record func(task.SessionIdentity) error) *liveSession {
	t.Helper()
	protocol, err := profile.Interaction(expected, record)
	if err != nil {
		t.Fatal(err)
	}
	live, ok := protocol.(recordProtocol)
	if !ok || !live.UseRecordCapture() {
		t.Fatalf("protocol %T does not use record capture", protocol)
	}
	session := &liveSession{protocol: live}
	frames, err := live.Start([]byte("brief"))
	if err != nil || len(frames) != 1 || !bytes.Contains(frames[0], []byte(`"method":"initialize"`)) {
		t.Fatalf("start frames=%q err=%v", frames, err)
	}
	return session
}

func TestFreshInteractiveLiveCaptureSurvivesStandardTrafficAndPublishesThroughReplay(t *testing.T) {
	for _, mode := range interactionModes {
		t.Run(mode, func(t *testing.T) {
			_, _, profile := interactiveRequest(t, mode)
			var recorded []task.SessionIdentity
			session := startLiveSession(t, profile, task.SessionExpectation{}, func(identity task.SessionIdentity) error {
				recorded = append(recorded, identity)
				return nil
			})
			trace := joinTrace(nativeSetupTrace(), standardTurnTrace())
			session.feedAll(t, trace[:len(trace)-1])
			if session.done || len(session.events) != 1 || session.events[0].Kind != "ready" || session.events[0].TurnID != "turn-1" {
				t.Fatalf("session closed or lost readiness before completion: done=%v events=%+v", session.done, session.events)
			}
			if len(session.outbound) != 3 || !bytes.Contains(session.outbound[2], []byte(`"method":"turn/start"`)) {
				t.Fatalf("outbound=%q", session.outbound)
			}
			if _, err := session.feed(trace[len(trace)-1]); err != nil || !session.done {
				t.Fatalf("matching completion did not finish the session: done=%v err=%v", session.done, err)
			}
			if err := session.protocol.Complete(); err != nil {
				t.Fatal(err)
			}
			if len(recorded) != 1 || recorded[0].ConversationID != testThreadID {
				t.Fatalf("recorded identity=%+v", recorded)
			}

			requireRegisteredReplayPublishes(t, profile.Plan.Predicate, trace)
		})
	}
}

// requireRegisteredReplayPublishes replays the sealed stdout through the
// interpreter the registration resolves for the profile's predicate.
func requireRegisteredReplayPublishes(t *testing.T, reference task.PredicateRef, trace []string) {
	t.Helper()
	registry, err := predicate.New(Registration().Interpreters...)
	if err != nil {
		t.Fatal(err)
	}
	interpreter, err := registry.Resolve(reference)
	if err != nil {
		t.Fatal(err)
	}
	stdout := []byte(strings.Join(trace, "\n") + "\n")
	seal := alteredSeal(stdout, nil, func(s *task.ProviderExitRecord) { s.Predicate = reference })
	var answer bytes.Buffer
	result, err := interpreter.Evaluate(predicate.Input{Seal: seal}, &testEvidence{stdout: stdout}, &answer)
	if err != nil || result.Verdict != task.VerdictCommitted || answer.String() != traceAnswer ||
		result.Session == nil || result.Session.ConversationID != testThreadID {
		t.Fatalf("replay result=%+v answer=%q err=%v", result, answer.String(), err)
	}
}

func TestLiveCaptureCannotCompleteFromUnrecognizedNotificationOrWrongTurn(t *testing.T) {
	_, _, profile := interactiveRequest(t, Mode)
	session := startLiveSession(t, profile, task.SessionExpectation{}, nil)
	session.feedAll(t, joinTrace(nativeSetupTrace(), coreTurnTrace()[:2]))
	session.feedAll(t, []string{rootNotification("turn/future", `,"turn":{"id":"turn-1","status":"completed"}`)})
	if session.done || session.protocol.Complete() == nil {
		t.Fatal("an unrecognized notification completed the turn")
	}
	if _, err := session.feed(rootTurnCompleted("turn-2", `"status":"completed"`)); !errors.Is(err, task.ErrInteractionConflict) {
		t.Fatalf("wrong-turn completion err=%v", err)
	}
	if _, err := session.feed(`{"method":"thread/future","method":"turn/completed","params":{}}`); err == nil {
		t.Fatal("duplicate method field accepted")
	}
}

func TestLiveCaptureDeniesEveryUnsupportedServerRequestWithAnID(t *testing.T) {
	for _, line := range []string{
		`{"id":5,"method":"item/commandExecution/requestApproval","params":{}}`,
		`{"id":"srv-1","method":"item/fileChange/requestApproval","params":{"threadId":"` + testThreadID + `"}}`,
		`{"id":9,"method":"thread/future","params":{}}`,
		`{"id":10,"method":"future/request"}`,
	} {
		_, _, profile := interactiveRequest(t, Mode)
		session := startLiveSession(t, profile, task.SessionExpectation{}, nil)
		session.feedAll(t, handledSetupTrace())
		before := len(session.outbound)
		update, err := session.feed(line)
		if err != nil || len(update.Events) != 0 || update.Done || len(update.Outbound) != 1 || len(session.outbound) != before+1 {
			t.Fatalf("%s update=%+v err=%v", line, update, err)
		}
		var denial map[string]json.RawMessage
		if err = json.Unmarshal(update.Outbound[0], &denial); err != nil || denial["error"] == nil || denial["result"] != nil {
			t.Fatalf("%s denial=%s err=%v", line, update.Outbound[0], err)
		}
		var request struct {
			ID json.RawMessage `json:"id"`
		}
		if err = json.Unmarshal([]byte(line), &request); err != nil || !bytes.Equal(denial["id"], request.ID) {
			t.Fatalf("%s denied id %s err=%v", line, denial["id"], err)
		}
		if session.protocol.Complete() == nil {
			t.Fatalf("%s completed the turn", line)
		}
	}
}

func TestLiveCaptureStillBridgesClarificationRequests(t *testing.T) {
	_, _, profile := interactiveRequest(t, Mode)
	session := startLiveSession(t, profile, task.SessionExpectation{}, nil)
	session.feedAll(t, joinTrace(nativeSetupTrace(), coreTurnTrace()[:1]))
	update, err := session.feed(`{"id":19,"method":"item/tool/requestUserInput","params":{"threadId":"` + testThreadID + `","turnId":"turn-1","isBlocking":true,"questions":[{"id":"color","question":"Choose","options":[{"label":"Blue"}]}]}}`)
	if err != nil || len(update.Events) != 1 || update.Events[0].Request == nil || !update.Events[0].Request.Blocking || len(update.Outbound) != 0 {
		t.Fatalf("clarification update=%+v err=%v", update, err)
	}
}

func TestContinuationLiveCaptureResumesPriorThreadAndKeepsIdentityStrict(t *testing.T) {
	request, now := profileReconstructionFixture(t)
	request.RequestedConfig.Interactive = true
	request.PriorSession = &task.PriorSession{Provider: Provider, ConversationID: testThreadID, PredecessorTaskID: testTaskID}
	candidate, err := PrepareCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	profile := finalizeProfile(t, candidate, now)
	if !profile.Plan.Predicate.Equal(interactionReference(Mode)) {
		t.Fatalf("continuation predicate=%+v", profile.Plan.Predicate)
	}
	session := startLiveSession(t, profile, task.SessionExpectation{Required: true, ID: testThreadID}, func(task.SessionIdentity) error { return nil })
	trace := joinTrace(nativeSetupTrace(), standardTurnTrace())
	session.feedAll(t, trace)
	if !session.done || session.protocol.Complete() != nil || !bytes.Contains(session.outbound[2], []byte(`"method":"turn/start"`)) {
		t.Fatalf("continuation did not complete: done=%v outbound=%q", session.done, session.outbound)
	}

	mismatched := startLiveSession(t, profile, task.SessionExpectation{Required: true, ID: testThreadID}, nil)
	mismatched.feedAll(t, handledSetupTrace()[:1])
	other := strings.Replace(handledSetupTrace()[3], testThreadID, "223e4567-e89b-12d3-a456-426614174000", 1)
	if _, err = mismatched.feed(other); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("resumed a different thread: %v", err)
	}
}

func TestSavedInteractiveTasksReconstructTheirOwnRevision(t *testing.T) {
	for _, mode := range interactionModes {
		request, now, fresh := interactiveRequest(t, mode)
		for _, revision := range interactionRevisions {
			meta := profileMeta(fresh)
			meta.Predicate = revision.reference(mode)
			candidate, err := PrepareExistingCandidate(request, meta)
			if err != nil {
				t.Fatalf("%s revision %d: %v", mode, revision, err)
			}
			profile := finalizeProfile(t, candidate, now)
			if !profile.Plan.Predicate.Equal(revision.reference(mode)) {
				t.Fatalf("%s revision %d reconstructed %+v", mode, revision, profile.Plan.Predicate)
			}
			protocol, err := profile.Interaction(task.SessionExpectation{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if live, ok := protocol.(execution.CurrentRecordInteractionProtocol); !ok || live.UseRecordCapture() != revision.usesRecordCapture() {
				t.Fatalf("%s revision %d record capture mismatch", mode, revision)
			}
			requireRevisionBehavior(t, revision, protocol)
		}
		meta := profileMeta(fresh)
		meta.Predicate = interactionReference(otherMode(mode))
		if _, err := PrepareExistingCandidate(request, meta); !errors.Is(err, ErrUnsupportedProfile) {
			t.Fatalf("interactive predicate of another mode was reused: %v", err)
		}
	}
}

// requireRevisionBehavior feeds the observed setup traffic, including the
// standard thread/started notification, and requires each frozen revision to
// keep its original verdict: only interactive-v2 treats thread/started as a
// protocol fault.
func requireRevisionBehavior(t *testing.T, revision interactionRevision, protocol execution.InteractionProtocol) {
	t.Helper()
	if _, err := protocol.Start([]byte("brief")); err != nil {
		t.Fatal(err)
	}
	for _, line := range nativeSetupTrace()[:5] {
		var err error
		if live, ok := protocol.(execution.CurrentRecordInteractionProtocol); ok && live.UseRecordCapture() {
			_, err = live.ReceiveRecord(providerjson.Value{Reader: strings.NewReader(line), End: int64(len(line))})
		} else {
			_, err = protocol.Receive([]byte(line))
		}
		faulted := revision == interactionStrict && strings.Contains(line, `"method":"thread/started"`)
		if faulted != errors.Is(err, task.ErrInteractionRequest) || (!faulted && err != nil) {
			t.Fatalf("revision %d on %s: %v", revision, line, err)
		}
	}
}

func otherMode(mode string) string {
	if mode == Mode {
		return WorkspaceWriteMode
	}
	return Mode
}
