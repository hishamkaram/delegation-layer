package tasks

import (
	"context"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func captureMessageID(a *app.Arguments) error {
	id, err := randomid.NewTaskID()
	if err == nil {
		a.MessageID = id
	}
	return err
}

func (v views) reply(id string) ui.Action {
	a := v.defaults("reply")
	a.TaskID = id
	return exactIDAction(ui.Action{
		ID: "task-reply", Label: "Reply to clarification", Description: "Answer a pending native request with an idempotent message. The task owner checks capability and request identity.", Arguments: a, Kind: session.Effect, Confirm: true, Capture: captureMessageID,
		Fields: []ui.Field{
			textField("Request ID", "copy the exact pending request from Inspect/events", "", func(a *app.Arguments, s string) { a.RequestID = s }),
			textField("Answers file", "choose a file or inline answers JSON", "", func(a *app.Arguments, s string) { a.Answers = s }),
			{Label: "Answers JSON", Hint: `JSON object keyed by question ID, e.g. {"color":["Blue"]}; choose JSON or a file`, Multiline: true, Apply: func(a *app.Arguments, s string) error {
				if s != "" {
					a.AnswersBytes = []byte(s)
				}
				return nil
			}},
		}, Reconciliation: func(a app.Arguments) []string { return readback(a, "events", a.TaskID) },
		Run: func(ctx context.Context, a app.Arguments) ui.Feedback { return taskFeedback(v.backend.Reply(ctx, a)) },
	}, id)
}

func (v views) steer(id string) ui.Action {
	a := v.defaults("steer")
	a.TaskID = id
	return exactIDAction(ui.Action{
		ID: "task-steer", Label: "Steer active turn", Description: "Send guidance only where the recorded active provider supports steering. No provider substitution.", Arguments: a, Fields: briefFields(), Kind: session.Effect, Confirm: true, Capture: captureMessageID,
		Reconciliation: func(a app.Arguments) []string { return readback(a, "events", a.TaskID) },
		Run:            func(ctx context.Context, a app.Arguments) ui.Feedback { return taskFeedback(v.backend.Steer(ctx, a)) },
	}, id)
}

func (v views) waitTasks() ui.Action {
	a := v.defaults("wait")
	return ui.Action{
		ID: "task-wait", Label: "Wait for task results", Description: "Finite passive any/all wait over up to32 unique local tasks. Undetermined liveness alone is not terminal evidence.", Arguments: a, Kind: session.Read,
		Fields: []ui.Field{
			{Label: "Task IDs", Hint: "one local ID per line or space; maximum32", Multiline: true, Apply: func(a *app.Arguments, s string) error { a.WaitTaskIDs = strings.Fields(s); return nil }},
			booleanField("Wait for all", "false returns when any task has terminal evidence", false, func(a *app.Arguments, b bool) { a.WaitAll = b }),
			durationField("Watch", a.Watch, func(a *app.Arguments, d time.Duration) { a.Watch = d }),
		}, Reconciliation: func(a app.Arguments) []string {
			result := []string{"delegate", "--root", a.Root, "wait", "--watch", "0s", "--json"}
			return append(result, a.WaitTaskIDs...)
		}, Run: func(ctx context.Context, a app.Arguments) ui.Feedback { return taskFeedback(v.backend.Wait(ctx, a)) },
	}
}
