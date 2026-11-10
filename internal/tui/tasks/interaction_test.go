package tasks

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	lifecyclefixture "github.com/hishamkaram/delegation-layer/internal/tasklifecycle/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func newInteractionView(t *testing.T, capabilities task.InteractionCapabilities) (views, *taskdir.TaskDir) {
	t.Helper()
	store, template, _ := lifecyclefixture.NewAppTestTask(t, false)
	request, metadata, err := template.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	brief, err := template.ReadBrief()
	if err != nil {
		t.Fatal(err)
	}
	if err = template.Close(); err != nil {
		t.Fatal(err)
	}
	request.TaskID = strings.Repeat("f", 32)
	request.RequestedConfig.Interactive = true
	metadata.TaskID, metadata.RequestedConfig = request.TaskID, request.RequestedConfig
	td, err := store.CreateTask(request.TaskID, request, brief, metadata)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := td.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	permit, err := td.PrepareStart(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := permit.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	if err = permit.Consume(); err != nil {
		t.Fatal(err)
	}
	for _, record := range []func() error{
		func() error { return td.RecordStarted(0) }, td.BeginInteraction,
		func() error {
			return td.RecordInteraction(task.InteractionEvent{Kind: "ready", TurnID: "native-turn", Capabilities: &capabilities})
		},
		func() error {
			return td.RecordInteraction(task.InteractionEvent{Kind: "request", Request: &task.InteractionRequest{ID: "native-request", TurnID: "native-turn", Blocking: true, Questions: []task.InteractionQuestion{{ID: "color", Text: "Choose a color", Options: []string{"Blue", "Red"}}}}})
		},
	} {
		if err = record(); err != nil {
			t.Fatal(err)
		}
	}
	var preparations atomic.Int32
	backend := app.NewWorkbench(app.Dependencies{Environment: []string{}, PrepareCandidate: func(task.TaskRecord, []string) (provider.ProfileCandidate, error) {
		preparations.Add(1)
		return provider.ProfileCandidate{}, errors.New("interaction must not prepare or launch a provider")
	}})
	before := interactionImmutableRecords(t, td)
	t.Cleanup(func() {
		if preparations.Load() != 0 {
			t.Error("interaction prepared a new provider turn")
		}
		if after := interactionImmutableRecords(t, td); !reflect.DeepEqual(after, before) {
			t.Error("interaction rewrote immutable task records")
		}
		entries, readErr := os.ReadDir(filepath.Join(store.Root, "tasks"))
		if readErr != nil || len(entries) != 2 {
			t.Errorf("interaction created or removed a task: entries=%d err=%v", len(entries), readErr)
		}
	})
	return views{backend: backend, base: app.Arguments{Root: store.Root}}, td
}

func interactionImmutableRecords(t *testing.T, td *taskdir.TaskDir) map[string]string {
	t.Helper()
	result := make(map[string]string)
	for _, name := range []string{"task.json", "meta.json", "brief.md", "provider.start", "provider.started.json"} {
		data, err := os.ReadFile(filepath.Join(td.Dir, name))
		if err != nil {
			t.Fatal(err)
		}
		result[name] = string(data)
	}
	return result
}

func interactionJournal(t *testing.T, td *taskdir.TaskDir) []task.InteractionEvent {
	t.Helper()
	events, _, err := td.InteractionEvents(0)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func replyExample(t *testing.T, action ui.Action) string {
	t.Helper()
	for _, field := range action.Fields {
		if field.Label == "Answers JSON" {
			first, last := strings.Index(field.Hint, "{"), strings.LastIndex(field.Hint, "}")
			if first < 0 || last < first {
				t.Fatalf("reply hint has no JSON object example: %q", field.Hint)
			}
			return field.Hint[first : last+1]
		}
	}
	t.Fatal("reply form has no inline answer field")
	return ""
}

func TestReplyFormExampleQueuesExactIdempotentNativeAnswers(t *testing.T) {
	v, td := newInteractionView(t, task.InteractionCapabilities{Clarification: true, Steering: true})
	action := v.reply(td.TaskID)
	a := prepare(t, action, map[string]string{"Request ID": "native-request", "Answers JSON": replyExample(t, action)})
	if action.Kind != session.Effect || !action.Confirm || task.ValidateTaskID(a.MessageID) != nil {
		t.Fatal("reply lost explicit effect confirmation or captured message identity")
	}
	for range 2 {
		if feedback := action.Run(t.Context(), a); feedback.Err != nil || feedback.Title != "reply" {
			t.Fatalf("reply/replay failed: %+v", feedback)
		}
	}
	events := interactionJournal(t, td)
	want := task.InteractionMessage{ID: a.MessageID, Kind: "reply", RequestID: "native-request", TurnID: "native-turn", Answers: map[string][]string{"color": {"Blue"}}}
	if len(events) != 3 || events[2].Message == nil || !reflect.DeepEqual(*events[2].Message, want) {
		t.Fatalf("form did not queue one exact reply: %+v", events)
	}
	a.AnswersBytes = []byte(`{"color":["Red"]}`)
	if feedback := action.Run(t.Context(), a); !errors.Is(feedback.Err, task.ErrInteractionConflict) {
		t.Fatalf("changed replay did not preserve its original message: %+v", feedback)
	}
	if !reflect.DeepEqual(interactionJournal(t, td), events) {
		t.Fatal("conflicting replay mutated the inbox")
	}
}

func TestSteerFormQueuesExactIdempotentNativeGuidance(t *testing.T) {
	v, td := newInteractionView(t, task.InteractionCapabilities{Clarification: true, Steering: true})
	action := v.steer(td.TaskID)
	a := prepare(t, action, map[string]string{"Brief text": "Keep the requested read-only scope."})
	if action.Kind != session.Effect || !action.Confirm || task.ValidateTaskID(a.MessageID) != nil {
		t.Fatal("steering lost explicit effect confirmation or captured message identity")
	}
	for range 2 {
		if feedback := action.Run(t.Context(), a); feedback.Err != nil || feedback.Title != "steer" {
			t.Fatalf("steer/replay failed: %+v", feedback)
		}
	}
	events := interactionJournal(t, td)
	want := task.InteractionMessage{ID: a.MessageID, Kind: "steer", TurnID: "native-turn", Text: "Keep the requested read-only scope."}
	if len(events) != 3 || events[2].Message == nil || !reflect.DeepEqual(*events[2].Message, want) {
		t.Fatalf("form did not queue one exact steering message: %+v", events)
	}
}

func TestInteractionFormsRefuseInvalidOrUnsupportedMessagesWithoutMutation(t *testing.T) {
	for _, test := range []struct {
		name     string
		steer    bool
		edits    map[string]string
		wantKind error
	}{
		{name: "answer array", edits: map[string]string{"Request ID": "native-request", "Answers JSON": `["Blue"]`}},
		{name: "stale request", edits: map[string]string{"Request ID": "stale-request", "Answers JSON": `{"color":["Blue"]}`}, wantKind: task.ErrInteractionRequest},
		{name: "unsupported steering", steer: true, edits: map[string]string{"Brief text": "Keep the scope."}, wantKind: task.ErrInteractionUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			v, td := newInteractionView(t, task.InteractionCapabilities{Clarification: true})
			action := v.reply(td.TaskID)
			if test.steer {
				action = v.steer(td.TaskID)
			}
			a := prepare(t, action, test.edits)
			before := interactionJournal(t, td)
			feedback := action.Run(t.Context(), a)
			if feedback.Err == nil || test.wantKind != nil && !errors.Is(feedback.Err, test.wantKind) {
				t.Fatalf("invalid interaction was not refused: %+v", feedback)
			}
			if !reflect.DeepEqual(interactionJournal(t, td), before) {
				t.Fatal("refused form changed interaction evidence")
			}
		})
	}
}
