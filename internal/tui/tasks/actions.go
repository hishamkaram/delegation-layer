package tasks

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func (v views) globalActions() []ui.Action {
	return []ui.Action{v.dispatch(false), v.dispatch(true), v.taskAction("status", ""), v.taskAction("collect", ""), v.result(""), v.logRange(""), v.taskAction("cancel", ""), v.continueTask(""), v.taskAction("events", ""), v.taskAction("logs", ""), v.reply(""), v.steer(""), v.taskAction("recheck", ""), v.waitTasks()}
}

func (v views) taskActions(id string) []ui.Action {
	result := []ui.Action{v.taskAction("status", id), v.taskAction("collect", id), v.result(id), v.logRange(id), v.taskAction("cancel", id), v.taskAction("events", id), v.taskAction("logs", id), v.reply(id), v.steer(id)}
	continuation := v.continueTask(id)
	recheck := v.taskAction("recheck", id)
	if strings.HasPrefix(id, "remote:") {
		continuation.Disabled = "Remote continuation is not supported by the existing contract."
		recheck.Disabled = "Remote recheck is not supported by the existing contract."
	}
	return append(result, continuation, recheck)
}

func (v views) taskAction(command, id string) ui.Action {
	a := v.defaults(command)
	a.TaskID = id
	result := ui.Action{ID: "task-" + command, Label: command, Arguments: a, Kind: session.Read, Reconciliation: func(a app.Arguments) []string { return readback(a, "status", a.TaskID) }}
	if id == "" {
		result.Fields = []ui.Field{taskIDField()}
	}
	switch command {
	case "status":
		result.Label, result.Kind = "Inspect live status", session.Effect
		result.Description = "Explicit status may contact the supervisor/remote and run existing maintenance. Automatic list refresh stays passive."
		result.Run = func(ctx context.Context, a app.Arguments) ui.Feedback { return taskFeedback(v.backend.Inspect(ctx, a)) }
	case "collect":
		result.Label, result.Kind = "Collect result", session.Effect
		result.Description = "Explicitly validate/recover publication. Pending and rejected are result states, not automatic retry requests."
		result.Fields = append(result.Fields, durationField("Watch", a.Watch, func(a *app.Arguments, d time.Duration) { a.Watch = d }))
		out := textField("Output directory", "remote collect only; optional explicit download destination", "", func(a *app.Arguments, s string) { a.OutputDirectory = s })
		result.Fields = append(result.Fields, advanced(out)...)
		result.Run = func(ctx context.Context, a app.Arguments) ui.Feedback { return taskFeedback(v.backend.Collect(ctx, a)) }
	case "cancel":
		result.Label, result.Kind, result.Confirm = "Cancel task", session.Effect, true
		result.Description = "Request the existing lifecycle owner's stop for exactly this task. Cancellation does not rewrite a committed outcome."
		result.Run = func(ctx context.Context, a app.Arguments) ui.Feedback { return taskFeedback(v.backend.Cancel(ctx, a)) }
	case "events":
		result.Label = "Interaction events"
		// Remote observation records/prunes origin evidence. Classification
		// uses the submitted ID too, including the exact-ID form.
		result.KindFor = func(a app.Arguments) session.Kind {
			if strings.HasPrefix(a.TaskID, "remote:") {
				return session.Effect
			}
			return session.Read
		}
		result.Fields = append(result.Fields,
			durationField("Watch", a.Watch, func(a *app.Arguments, d time.Duration) { a.Watch = d }),
			ui.Field{Label: "After cursor", Hint: "existing event cursor; 0 starts at the beginning", Value: "0", Apply: func(a *app.Arguments, s string) error {
				value, err := strconv.ParseUint(s, 10, 64)
				if err == nil {
					a.After = value
				}
				return err
			}})
		result.Run = func(ctx context.Context, a app.Arguments) ui.Feedback { return taskFeedback(v.backend.Events(ctx, a)) }
	case "logs":
		result.Label = "Log descriptors"
		result.Description = "Validated output descriptors. Provider log content is opened separately and is never indexed."
		result.Run = func(ctx context.Context, a app.Arguments) ui.Feedback { return taskFeedback(v.backend.Logs(ctx, a)) }
	case "recheck":
		result.Label, result.Kind = "Recheck publication", session.Effect
		result.Description = "Use only the existing eligible recovery contract; never rerun the provider."
		result.Run = func(_ context.Context, a app.Arguments) ui.Feedback { return taskFeedback(v.backend.Recheck(a)) }
	}
	if id == "" {
		result.Label += " by ID"
	}
	return result
}

func taskFeedback(result app.OperationResult) ui.Feedback {
	feedback := ui.Result(result)
	var data []byte
	var err error
	if result.Remote != nil {
		data, err = json.MarshalIndent(result.Remote, "", "  ")
	} else {
		data, err = json.MarshalIndent(result.Response, "", "  ")
	}
	if err != nil {
		feedback.Err = err
		return feedback
	}
	feedback.Body += "\n\nControl details (result/log bytes are separate):\n" + string(data)
	return feedback
}
