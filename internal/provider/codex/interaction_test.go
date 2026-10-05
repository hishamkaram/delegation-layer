package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func initializedAppServer(t *testing.T) *appServerProtocol {
	t.Helper()
	request := task.TaskRecord{Mode: Mode, CanonicalCwd: "/workspace", RequestedConfig: task.TaskConfig{Model: "requested-model", Effort: "max"}}
	protocol, err := newAppServerProtocol(request, task.SessionExpectation{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := protocol.(*appServerProtocol)
	if !ok {
		t.Fatal("wrong protocol")
	}
	if _, err = p.Start([]byte("literal brief")); err != nil {
		t.Fatal(err)
	}
	for _, line := range appServerSetup() {
		if _, err = p.Receive([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func appServerSetup() []string {
	return []string{
		`{"id":"delegate-init","result":{}}`,
		fmt.Sprintf(`{"id":"delegate-thread","result":{"thread":{"id":%q}}}`, testThreadID),
		`{"id":"delegate-turn","result":{"turn":{"id":"turn-1"}}}`,
	}
}

func receiveAppServer(t *testing.T, p *appServerProtocol, line string) execution.InteractionUpdate {
	t.Helper()
	update, err := p.Receive([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	return update
}

func TestAppServerClarificationPreservesNativeIDAndDoesNotInventAcknowledgement(t *testing.T) {
	p := initializedAppServer(t)
	update := receiveAppServer(t, p, fmt.Sprintf(`{"id":19,"method":"item/tool/requestUserInput","params":{"threadId":%q,"turnId":"turn-1","isBlocking":true,"questions":[{"id":"color","question":"Choose a color","isOther":true,"options":[{"label":"Blue"}]}]}}`, testThreadID))
	if len(update.Events) != 1 || update.Events[0].Request == nil {
		t.Fatal("missing request")
	}
	request := update.Events[0].Request
	frame, err := p.Encode(task.InteractionMessage{ID: strings.Repeat("a", 32), Kind: "reply", TurnID: "turn-1", RequestID: request.ID, Answers: map[string][]string{"color": {"Blue"}}})
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		ID     int `json:"id"`
		Result struct {
			Answers map[string]struct {
				Answers []string `json:"answers"`
			} `json:"answers"`
		} `json:"result"`
	}
	if err = json.Unmarshal(frame, &response); err != nil {
		t.Fatal(err)
	}
	if response.ID != 19 || len(response.Result.Answers["color"].Answers) != 1 || response.Result.Answers["color"].Answers[0] != "Blue" {
		t.Fatalf("invalid native response: %s", frame)
	}
	update = receiveAppServer(t, p, fmt.Sprintf(`{"method":"serverRequest/resolved","params":{"threadId":%q,"requestId":19}}`, testThreadID))
	if len(update.Events) != 1 || update.Events[0].Kind != "resolved" {
		t.Fatalf("resolution invented delivery acknowledgement: %+v", update)
	}
}

func TestAppServerSteeringBindsActiveTurnAndAcknowledgesNativeResponse(t *testing.T) {
	p := initializedAppServer(t)
	id := strings.Repeat("b", 32)
	frame, err := p.Encode(task.InteractionMessage{ID: id, Kind: "steer", TurnID: "turn-1", Text: "Focus on latency"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(frame, []byte(`"expectedTurnId":"turn-1"`)) || !bytes.Contains(frame, []byte(`"method":"turn/steer"`)) {
		t.Fatalf("missing active-turn precondition: %s", frame)
	}
	update := receiveAppServer(t, p, fmt.Sprintf(`{"id":"delegate-steer-%s","result":{"turnId":"turn-1"}}`, id))
	if len(update.Events) != 1 || update.Events[0].Delivery.State != "acknowledged" {
		t.Fatalf("native response lost: %+v", update)
	}
	if _, err = p.Encode(task.InteractionMessage{ID: id, Kind: "steer", TurnID: "old-turn", Text: "late"}); err == nil {
		t.Fatal("stale steering accepted")
	}
}

func TestAppServerNeverApprovesNativeToolRequests(t *testing.T) {
	p := initializedAppServer(t)
	update := receiveAppServer(t, p, `{"id":5,"method":"item/commandExecution/requestApproval","params":{}}`)
	if len(update.Events) != 0 || len(update.Outbound) != 1 || !bytes.Contains(update.Outbound[0], []byte(`"error"`)) {
		t.Fatalf("tool permission exposed as clarification: %+v", update)
	}
}

func TestAppServerSealedResultRequiresRootCompletion(t *testing.T) {
	lines := appServerSetup()
	lines = append(lines, fmt.Sprintf(`{"method":"item/completed","params":{"threadId":%q,"turnId":"turn-1","item":{"type":"agentMessage","text":"answer","phase":"final_answer"}}}`, testThreadID))
	complete := fmt.Sprintf(`{"method":"turn/completed","params":{"threadId":%q,"turn":{"id":"turn-1","status":"completed"}}}`, testThreadID)
	for _, tc := range []struct {
		name  string
		extra []string
		want  string
	}{
		{"success", []string{complete}, task.VerdictCommitted},
		{"incomplete", nil, task.VerdictRejected},
		{"duplicate completion", []string{complete, complete}, task.VerdictRejected},
		{"fatal error before success", []string{fmt.Sprintf(`{"method":"error","params":{"threadId":%q,"turnId":"turn-1","willRetry":false,"error":{"message":"failed"}}}`, testThreadID), complete}, task.VerdictRejected},
		{"native transient retry", []string{fmt.Sprintf(`{"method":"error","params":{"threadId":%q,"turnId":"turn-1","willRetry":true,"error":{"message":"retrying"}}}`, testThreadID), complete}, task.VerdictCommitted},
		{"conflicting completion error", []string{strings.Replace(complete, `"status":"completed"`, `"status":"completed","error":{"message":"failed"}`, 1)}, task.VerdictRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout := []byte(strings.Join(append(append([]string{}, lines...), tc.extra...), "\n") + "\n")
			seal := validSeal(stdout, nil)
			seal.Predicate = interactionReference(Mode)
			var answer bytes.Buffer
			result, err := (interactionInterpreter{mode: Mode}).Evaluate(predicate.Input{Seal: seal}, &testEvidence{stdout: stdout}, &answer)
			if err != nil || result.Verdict != tc.want {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}
