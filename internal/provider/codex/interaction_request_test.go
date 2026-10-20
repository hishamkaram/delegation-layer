package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// withID returns the same JSON-RPC envelope carrying a request id, which makes
// it a request by JSON-RPC classification whatever its method name is.
func withID(t *testing.T, id, envelope string) string {
	t.Helper()
	const open = `{"method":`
	if !strings.HasPrefix(envelope, open) {
		t.Fatalf("not a method envelope: %s", envelope)
	}
	return fmt.Sprintf(`{"id":%s,%s`, id, strings.TrimPrefix(envelope, "{"))
}

// knownNotificationsWithID are the notification names the parser interprets,
// each in a shape that would change lifecycle state if it were a notification.
func knownNotificationsWithID(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"turn/completed":         withID(t, "7", rootTurnCompleted("turn-1", `"status":"completed"`)),
		"turn/started":           withID(t, "7", rootNotification("turn/started", `,"turn":{"id":"turn-1","status":"inProgress"}`)),
		"item/completed":         withID(t, `"srv-7"`, finalMessage("item-1", traceAnswer)),
		"error":                  withID(t, "7", rootNotification("error", `,"turnId":"turn-1","willRetry":false,"error":{"message":"failed"}`)),
		"serverRequest/resolved": withID(t, "7", rootNotification("serverRequest/resolved", `,"requestId":19`)),
	}
}

func requireDeniedByID(t *testing.T, session *liveSession, line, wantID string) {
	t.Helper()
	update, err := session.feed(line)
	if err != nil || update.Done || len(update.Events) != 0 || len(update.Outbound) != 1 {
		t.Fatalf("%s update=%+v err=%v", line, update, err)
	}
	var denial map[string]json.RawMessage
	if err = json.Unmarshal(update.Outbound[0], &denial); err != nil || denial["error"] == nil || denial["result"] != nil || string(denial["id"]) != wantID {
		t.Fatalf("%s denial=%s err=%v", line, update.Outbound[0], err)
	}
}

func TestCurrentLiveCaptureDeniesKnownNotificationNamesCarryingAnID(t *testing.T) {
	for name, line := range knownNotificationsWithID(t) {
		t.Run(name, func(t *testing.T) {
			_, _, profile := interactiveRequest(t, Mode)
			session := startLiveSession(t, profile, task.SessionExpectation{}, nil)
			session.feedAll(t, joinTrace(handledSetupTrace(), coreTurnTrace()[:1]))
			wantID := "7"
			if name == "item/completed" {
				wantID = `"srv-7"`
			}
			requireDeniedByID(t, session, line, wantID)
			if session.done || session.protocol.Complete() == nil {
				t.Fatal("a request completed or failed the turn")
			}
			// Nothing was authorized or recorded, so the genuine
			// notifications still decide the turn.
			session.feedAll(t, coreTurnTrace()[1:])
			if !session.done || session.protocol.Complete() != nil {
				t.Fatalf("matching completion no longer completes: done=%v", session.done)
			}
		})
	}
}

func TestFrozenStrictRevisionStillTreatsNamedRequestsAsNotifications(t *testing.T) {
	request, now, fresh := interactiveRequest(t, Mode)
	profile := savedProfile(t, request, now, fresh, interactionStrict)
	session := startLiveSession(t, profile, task.SessionExpectation{}, nil)
	session.feedAll(t, joinTrace(handledSetupTrace(), coreTurnTrace()[:1]))
	update, err := session.feed(withID(t, "7", rootTurnCompleted("turn-1", `"status":"completed"`)))
	if err != nil || !update.Done || len(update.Outbound) != 0 {
		t.Fatalf("interactive-v2 changed: update=%+v err=%v", update, err)
	}
}

func savedProfile(t *testing.T, request task.TaskRecord, now time.Time, fresh commonprovider.PreparedProfile, revision interactionRevision) commonprovider.PreparedProfile {
	t.Helper()
	meta := profileMeta(fresh)
	meta.Predicate = revision.reference(request.Mode)
	candidate, err := PrepareExistingCandidate(request, meta)
	if err != nil {
		t.Fatal(err)
	}
	return finalizeProfile(t, candidate, now)
}

func TestCurrentLiveCaptureRejectsKnownRequestWithoutID(t *testing.T) {
	question := `"params":{"threadId":"` + testThreadID + `","turnId":"turn-1","isBlocking":true,"questions":[{"id":"color","question":"Choose"}]}`
	otherThread := strings.Replace(question, testThreadID, "other-thread", 1)
	for name, line := range map[string]string{
		"root thread":  `{"method":"item/tool/requestUserInput",` + question + `}`,
		"other thread": `{"method":"item/tool/requestUserInput",` + otherThread + `}`,
	} {
		_, _, profile := interactiveRequest(t, Mode)
		session := startLiveSession(t, profile, task.SessionExpectation{}, nil)
		session.feedAll(t, joinTrace(handledSetupTrace(), coreTurnTrace()[:1]))
		before := len(session.outbound)
		update, err := session.feed(line)
		if !errors.Is(err, task.ErrInteractionRequest) || len(update.Outbound) != 0 || len(session.outbound) != before {
			t.Fatalf("%s update=%+v err=%v", name, update, err)
		}
	}
}

func TestCurrentLiveCaptureRejectsMalformedIDTypes(t *testing.T) {
	params := `"params":{"threadId":"` + testThreadID + `","turnId":"turn-1","isBlocking":true,"questions":[{"id":"color","question":"Choose"}]}`
	for _, id := range []string{`null`, `true`, `{}`, `[]`, `{"a":1}`} {
		for name, envelope := range map[string]string{
			"known notification": fmt.Sprintf(`{"id":%s,"method":"turn/completed","params":{"threadId":%q,"turn":{"id":"turn-1","status":"completed"}}}`, id, testThreadID),
			"clarification":      fmt.Sprintf(`{"id":%s,"method":"item/tool/requestUserInput",%s}`, id, params),
			"unknown request":    fmt.Sprintf(`{"id":%s,"method":"future/request"}`, id),
		} {
			_, _, profile := interactiveRequest(t, Mode)
			session := startLiveSession(t, profile, task.SessionExpectation{}, nil)
			session.feedAll(t, joinTrace(handledSetupTrace(), coreTurnTrace()[:1]))
			before := len(session.outbound)
			update, err := session.feed(envelope)
			if !errors.Is(err, task.ErrInteractionRequest) || update.Done || len(session.outbound) != before {
				t.Fatalf("%s id %s update=%+v err=%v", name, id, update, err)
			}
		}
	}
}

func TestCurrentLiveCaptureStillAcceptsStringAndNumberRequestIDs(t *testing.T) {
	params := `"params":{"threadId":"` + testThreadID + `","turnId":"turn-1","isBlocking":true,"questions":[{"id":"color","question":"Choose"}]}`
	for _, id := range []string{`19`, `"srv-19"`, `1.5`} {
		_, _, profile := interactiveRequest(t, Mode)
		session := startLiveSession(t, profile, task.SessionExpectation{}, nil)
		session.feedAll(t, joinTrace(handledSetupTrace(), coreTurnTrace()[:1]))
		update, err := session.feed(fmt.Sprintf(`{"id":%s,"method":"item/tool/requestUserInput",%s}`, id, params))
		if err != nil || len(update.Events) != 1 || update.Events[0].Request == nil || len(update.Outbound) != 0 {
			t.Fatalf("id %s update=%+v err=%v", id, update, err)
		}
	}
}

func TestCurrentLiveCaptureRejectsDuplicateSelectedEnvelopeFields(t *testing.T) {
	completed := fmt.Sprintf(`"params":{"threadId":%q,"turn":{"id":"turn-1","status":"completed"}}`, testThreadID)
	for name, line := range map[string]string{
		"duplicate id":                `{"id":7,"id":8,"method":"turn/completed",` + completed + `}`,
		"duplicate method with id":    `{"id":7,"method":"turn/completed","method":"future/request",` + completed + `}`,
		"duplicate params with id":    `{"id":7,"method":"future/request","params":{},"params":{}}`,
		"id and method case variants": `{"ID":7,"id":8,"method":"turn/completed",` + completed + `}`,
	} {
		_, _, profile := interactiveRequest(t, Mode)
		session := startLiveSession(t, profile, task.SessionExpectation{}, nil)
		session.feedAll(t, joinTrace(handledSetupTrace(), coreTurnTrace()[:1]))
		update, err := session.feed(line)
		if err == nil || update.Done || len(update.Outbound) != 0 {
			t.Fatalf("%s update=%+v err=%v", name, update, err)
		}
	}
}

func TestKnownNotificationNamesWithIDNeverAuthorizeReplayCommitment(t *testing.T) {
	notifications := knownNotificationsWithID(t)
	completedWithID := notifications["turn/completed"]
	finalWithID := notifications["item/completed"]
	setup := handledSetupTrace()
	started := coreTurnTrace()[0]
	final, completed := coreTurnTrace()[1], coreTurnTrace()[2]
	for name, test := range map[string]struct {
		lines []string
		want  string
	}{
		"completion request only":            {joinTrace(setup, []string{started, final, completedWithID}), task.VerdictRejected},
		"final request then real completion": {joinTrace(setup, []string{started, finalWithID, completed}), task.VerdictRejected},
		"completion request then real one":   {joinTrace(setup, []string{started, final, completedWithID, completed}), task.VerdictCommitted},
		"failure request then real success":  {joinTrace(setup, []string{started, final, notifications["error"], completed}), task.VerdictCommitted},
		"known request without id":           {joinTrace(setup, []string{started, final, `{"method":"item/tool/requestUserInput","params":{}}`, completed}), task.VerdictRejected},
		"unrecognized notification only":     {joinTrace(setup, []string{started, final, rootNotification("turn/future", `,"turn":{"id":"turn-1","status":"completed"}`)}), task.VerdictRejected},
	} {
		t.Run(name, func(t *testing.T) {
			requireTrace(t, traceRun{revision: interactionCurrent, lines: test.lines}, test.want)
		})
	}
	// The frozen revision keeps treating the named request as a notification.
	requireTrace(t, traceRun{revision: interactionStrict, lines: joinTrace(setup, []string{started, final, completedWithID})}, task.VerdictCommitted)
}

func TestCurrentReplayStillRequiresTheMatchingCompletionAndFinal(t *testing.T) {
	setup := handledSetupTrace()
	core := coreTurnTrace()
	denied := withID(t, "9", rootNotification("future/event", ""))
	requireTrace(t, traceRun{revision: interactionCurrent, lines: joinTrace(setup, core[:1], []string{denied}, core[1:])}, task.VerdictCommitted)
	wrongTurn := joinTrace(setup, core[:2], []string{withID(t, "9", core[2]), rootTurnCompleted("turn-2", `"status":"completed"`)})
	requireTrace(t, traceRun{revision: interactionCurrent, lines: wrongTurn}, task.VerdictRejected)
}
