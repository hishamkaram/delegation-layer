package app

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

func TestInteractionCommandArguments(t *testing.T) {
	id := strings.Repeat("a", 32)
	for _, args := range [][]string{
		{"dispatch", "--provider", "codex:exec", "--cwd", "/workspace", "--brief", "/brief", "--interactive", "--json"},
		{"preflight", "--provider", "claude:print", "--cwd", "/workspace", "--interactive", "--json"},
		{"continue", "--task", id, "--interactive", "--json"},
	} {
		parsed, err := ParseArguments(args)
		if err != nil || !parsed.Config.Interactive {
			t.Fatalf("interaction not parsed: %+v %v", parsed, err)
		}
	}
	reply, err := ParseArguments([]string{"reply", id, "--request", "question-1", "--answers", "answers.json", "--id", id, "--json"})
	if err != nil || reply.RequestID != "question-1" || reply.MessageID != id {
		t.Fatalf("reply: %+v %v", reply, err)
	}
	events, err := ParseArguments([]string{"events", id, "--after", "100", "--watch", "3s", "--json"})
	if err != nil || events.After != 100 || events.Watch != 3*time.Second {
		t.Fatalf("events: %+v %v", events, err)
	}
	for _, args := range [][]string{
		{"reply", id, "--request", "question", "--answers", "a.json"},
		{"reply", id, "--id", id, "--answers", "a.json"},
		{"steer", id, "--id", id},
		{"steer", id, "--brief", "/brief", "--id", "invalid"},
		{"events", id, "--after", "-1"},
		{"events", id, "--watch", "-1s"},
	} {
		if _, err := ParseArguments(args); err == nil {
			t.Fatalf("invalid command accepted: %v", args)
		}
	}
}

func TestInteractionContinuationPreservesOptIn(t *testing.T) {
	for _, pair := range [][2]bool{{false, false}, {true, false}, {false, true}, {true, true}} {
		got := tasklifecycle.ContinuationConfig(task.TaskConfig{Interactive: pair[0], Permission: "read-only", Model: "chosen"}, task.TaskConfig{Interactive: pair[1]})
		if got.Interactive != (pair[0] || pair[1]) || got.Model != "chosen" || got.Permission != "read-only" {
			t.Fatalf("continuation changed authority: %+v", got)
		}
	}
}

func newAppInteractiveTask(t *testing.T) (*taskdir.Store, *taskdir.TaskDir, *taskdir.StartPermit) {
	t.Helper()
	store, source, _ := newAppTestTask(t, false)
	req, meta, err := source.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	brief, err := source.ReadBrief()
	if err != nil {
		t.Fatal(err)
	}
	if err = source.Close(); err != nil {
		t.Fatal(err)
	}
	req.TaskID = strings.Repeat("f", 32)
	req.RequestedConfig.Interactive = true
	meta.TaskID = req.TaskID
	meta.RequestedConfig = req.RequestedConfig
	td, err := store.CreateTask(req.TaskID, req, brief, meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	permit, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	for _, fn := range []func() error{func() error { return td.RecordStarted(0) }, td.BeginInteraction, func() error {
		return td.RecordInteraction(task.InteractionEvent{Kind: "ready", TurnID: "turn", Capabilities: &task.InteractionCapabilities{Clarification: true}})
	}, func() error {
		return td.RecordInteraction(task.InteractionEvent{Kind: "request", Request: &task.InteractionRequest{ID: "question", TurnID: "turn", Blocking: true, Questions: []task.InteractionQuestion{{ID: "color", Text: "Choose", Options: []string{"Blue", "Red"}}}}})
	}} {
		if err = fn(); err != nil {
			t.Fatal(err)
		}
	}
	return store, td, permit
}

func TestInteractionCollectionReturnsQuestionAndReplyIsIdempotent(t *testing.T) {
	store, td, _ := newAppInteractiveTask(t)
	result := collect(Arguments{Root: store.Root, TaskID: td.TaskID, Watch: time.Minute}, storeDependencies{})
	if result.code != 3 || result.err != nil || result.response.Pending == nil || result.response.Pending.Kind != "interaction" {
		t.Fatalf("question not returned: %+v", result)
	}
	file := filepath.Join(t.TempDir(), "answers.json")
	writeAppTestFile(t, file, []byte(`{"color":["Blue"]}`))
	args := Arguments{Command: "reply", Root: store.Root, TaskID: td.TaskID, RequestID: "question", MessageID: strings.Repeat("b", 32), Answers: file}
	first := interactionCommand(args, storeDependencies{})
	second := interactionCommand(args, storeDependencies{})
	if first.err != nil || second.err != nil || first.response.Delivery == nil || second.response.Delivery.State != "queued" {
		t.Fatalf("reply/replay: %+v %+v", first, second)
	}
	writeAppTestFile(t, file, []byte(`{"color":["Red"]}`))
	conflict := interactionCommand(args, storeDependencies{})
	if !errors.Is(conflict.err, task.ErrInteractionConflict) {
		t.Fatalf("changed content not rejected: %+v", conflict)
	}
}

func TestInteractionJournalCannotHideTerminalOutcome(t *testing.T) {
	store, td, permit := newAppInteractiveTask(t)
	if err := td.WriteRawFiles("answer\n", ""); err != nil {
		t.Fatal(err)
	}
	if err := td.SealInteraction(); err != nil {
		t.Fatal(err)
	}
	if _, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef()); err != nil {
		t.Fatal(err)
	}
	if err := permit.Release(); err != nil {
		t.Fatal(err)
	}
	outcome, cleanup, err := td.Collect(task.FixturePredicateRef())
	if outcome == nil || errors.Join(cleanup, err) != nil {
		t.Fatalf("fixture publication: %v %v", cleanup, err)
	}
	writeAppTestFile(t, filepath.Join(td.Dir, "interaction", "00000000000000000001.json"), []byte("corrupt optional live journal"))
	result := collect(Arguments{Root: store.Root, TaskID: td.TaskID}, storeDependencies{})
	if result.err != nil || result.response.Outcome == nil || result.response.Outcome.Verdict != outcome.Verdict {
		t.Fatalf("journal hid winner: %+v", result)
	}
	observed := status(Arguments{Root: store.Root, TaskID: td.TaskID}, Dependencies{})
	if observed.err != nil || observed.response.Outcome == nil || observed.response.Outcome.Verdict != outcome.Verdict {
		t.Fatalf("status journal hid winner: %+v", observed)
	}
}

func TestInteractionEventPageFitsCompleteResponse(t *testing.T) {
	_, td, _ := newAppInteractiveTask(t)
	for index := range 3 {
		_, err := td.SubmitInteraction(task.InteractionMessage{ID: fmt.Sprintf("%032x", index), Kind: "reply", RequestID: "question", Answers: map[string][]string{"color": {"Blue"}}})
		if index == 0 && err != nil {
			t.Fatal(err)
		}
		// Only one answer may target the pending batch; later IDs must conflict.
		if index > 0 && !errors.Is(err, task.ErrInteractionConflict) {
			t.Fatalf("competing reply accepted: %v", err)
		}
	}
	request := task.InteractionRequest{ID: "large", TurnID: "turn", Questions: []task.InteractionQuestion{{ID: "long", Text: strings.Repeat("x", 200*1024), FreeText: true}}}
	if err := td.RecordInteraction(task.InteractionEvent{Kind: "request", Request: &request}); err != nil {
		t.Fatal(err)
	}
	response := newResponse("events")
	if err := fillInteraction(&response, td); err != nil {
		t.Fatal(err)
	}
	events, cursor, err := td.InteractionEvents(0)
	if err != nil {
		t.Fatal(err)
	}
	response.Events = events
	response.Cursor = &cursor
	var buffer bytes.Buffer
	if err = writeJSONResponse(&buffer, "events", &response, nil); err != nil {
		t.Fatalf("valid state/page not encodable: %v", err)
	}
	if buffer.Len() > task.MaxControlRecordSize {
		t.Fatalf("response too large: %d", buffer.Len())
	}
}

func TestInteractionHumanOutputIncludesQuestionsReceiptsAndCursor(t *testing.T) {
	cursor := uint64(3)
	delivery := task.InteractionDelivery{MessageID: strings.Repeat("a", 32), State: "sent"}
	response := newResponse("events")
	response.Cursor = &cursor
	response.Delivery = &delivery
	response.Interaction = &task.InteractionState{TurnID: "turn", PendingRequests: []task.InteractionRequest{{ID: "request", Questions: []task.InteractionQuestion{{ID: "color", Text: "Choose a color", Options: []string{"Blue", "Red"}}}}}}
	response.Events = []task.InteractionEvent{{Sequence: 3, Kind: "delivery", Delivery: &delivery}}
	var output bytes.Buffer
	if err := writeHumanResponse(&output, response); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`turn_id="turn"`, `request="request"`, `question="color"`, `text="Choose a color"`, `options=["Blue" "Red"]`, `state=sent`, `event={`, `"message_id":"` + delivery.MessageID + `"`, `cursor=3`} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("human response omitted %q: %s", want, output.String())
		}
	}
}
