package schedules

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func (v views) loadSchedules(ctx context.Context, page ui.PageRequest) (ui.Page, error) {
	a := v.defaults("schedule", "list")
	a.ScheduleAfter = page.After
	response := v.backend.Schedules(ctx, a)
	if response.Err != nil {
		return ui.Page{}, response.Err
	}
	result := ui.Page{Next: response.Response.ScheduleCursor, Summary: "Immutable definitions; search filters this page. Host readiness is separate from schedule health."}
	if response.Response.Schedules == nil {
		return result, nil
	}
	for _, row := range *response.Response.Schedules {
		if !matching(page.Search, row.ID, string(row.State), row.Cron, row.Timezone) {
			continue
		}
		result.Entries = append(result.Entries, ui.Entry{
			ID: row.ID, Title: row.ID, Subtitle: fmt.Sprintf("%s • %s • %s", row.State, row.Cron, row.Timezone),
			Detail:  fmt.Sprintf("Schedule: %s\nState: %s\nCron: %s\nTimezone: %s\nCreated: %s\n\nPause/delete never cancel an already admitted task.", row.ID, row.State, row.Cron, row.Timezone, row.CreatedAt.Format(time.RFC3339)),
			Actions: v.scheduleActions(row.ID),
		})
	}
	return result, nil
}

func (v views) scheduleActions(id string) []ui.Action {
	var result []ui.Action
	for _, action := range []string{"show", "health", "history", "pause", "resume", "delete"} {
		a := v.defaults("schedule", action)
		a.ScheduleID = id
		item := ui.Action{ID: "schedule-" + action, Label: action, Arguments: a, Kind: session.Read, Run: v.runSchedule, Reconciliation: scheduleReadback}
		if action == "pause" || action == "resume" || action == "delete" {
			item.Kind = session.Effect
			item.Confirm = true
			item.Description = "Changes future admissions only; active tasks remain supervised. Resume also clears an active quota hold."
		}
		if action == "history" {
			item.Fields = []ui.Field{ui.TextField("Before", "optional RFC3339Nano exclusive cursor", "", func(a *app.Arguments, s string) { a.ScheduleBefore = s })}
		}
		result = append(result, item)
	}
	get := v.defaults("schedule", "policy", "get")
	get.ScheduleID = id
	set := v.defaults("schedule", "policy", "set")
	set.ScheduleID = id
	result = append(result, ui.Action{ID: "schedule-policy-get", Label: "Quota policy", Arguments: get, Kind: session.Read, Run: v.runSchedule, Reconciliation: scheduleReadback},
		ui.Action{ID: "schedule-policy-set", Label: "Set quota policy", Arguments: set, Kind: session.Effect, Confirm: true, Description: "Change explicit admission-hold policy, not the immutable schedule definition.", Run: v.runSchedule, Reconciliation: scheduleReadback, Fields: []ui.Field{
			ui.TextField("Quota hold", "on or off; blank preserves current value", "", func(a *app.Arguments, s string) { a.ScheduleQuotaHold = s }),
			ui.TextField("Maximum hold", "duration; blank preserves current value", "", func(a *app.Arguments, s string) { a.ScheduleQuotaMaxHold = s }),
		}})
	return result
}

func (v views) createSchedule() ui.Action {
	a := v.defaults("schedule", "create")
	fields := []ui.Field{
		ui.TextField("Working directory", "absolute provider workspace", v.cwd, func(a *app.Arguments, s string) { a.Cwd = s }),
		ui.TextField("Cron", "five fields, e.g. 0 */2 * * *", "", func(a *app.Arguments, s string) { a.ScheduleCron = s }),
		ui.TextField("Timezone", "IANA zone; blank uses the scheduler default", "", func(a *app.Arguments, s string) { a.ScheduleTimezone = s; a.ScheduleTimezoneSet = s != "" }),
		ui.TextField("Provider", "exact provider; leave blank for auto or Team", "", func(a *app.Arguments, s string) { a.Provider = s }),
		{Label: "Auto", Hint: "true or false; mutually exclusive with provider", Value: "false", Apply: func(a *app.Arguments, s string) error {
			value, err := strconv.ParseBool(s)
			if err == nil {
				a.Auto = value
			}
			return err
		}},
		ui.TextField("Brief file", "choose a file or inline text", "", func(a *app.Arguments, s string) { a.Brief = s }),
		{Label: "Brief text", Hint: "choose text or a file", Multiline: true, Apply: func(a *app.Arguments, s string) error {
			if s != "" {
				a.BriefBytes = []byte(s)
			}
			return nil
		}},
	}
	fields = append(fields, scheduleExecutionFields()...)
	fields = append(fields, scheduleHookFields()...)
	return ui.Action{
		ID: "schedule-create", Label: "Create schedule", Arguments: a, Fields: fields, Kind: session.Effect, Confirm: true,
		Description: "Save an immutable recurring definition. This does not start a scheduler host or provider.", Capture: func(a *app.Arguments) error {
			id, err := randomid.NewTaskID()
			if err == nil {
				a.ScheduleID = id
			}
			return err
		},
		Reconciliation: scheduleReadback, Run: v.runSchedule,
	}
}

func scheduleExecutionFields() []ui.Field {
	return []ui.Field{
		ui.TextField("Model", "exact requested model; blank keeps provider/Team default", "", func(a *app.Arguments, s string) { a.Config.Model = s }),
		ui.TextField("Effort", "exact provider-supported effort; blank keeps default", "", func(a *app.Arguments, s string) { a.Config.Effort = s }),
		ui.TextField("Permission", "native permission intent; blank keeps owner default", "", func(a *app.Arguments, s string) { a.Config.Permission = s }),
		ui.TextField("Task budget", "duration; blank uses owner default", "", func(a *app.Arguments, s string) { a.Config.Budget = s }),
		ui.TextField("Output silence timeout", "opt-in duration; blank disables", "", func(a *app.Arguments, s string) { a.Config.OutputSilenceTimeout = s }),
		ui.TextField("Native timeout", "optional provider-native timeout", "", func(a *app.Arguments, s string) { a.Config.NativeTimeout = s }),
		ui.TextField("Review target", "uncommitted, base:BRANCH or commit:SHA", "", func(a *app.Arguments, s string) { a.ReviewTarget = s }),
		ui.TextField("Team scope", "optional user or project", "", func(a *app.Arguments, s string) { a.TeamScope = s }),
		ui.TextField("Team ID", "explicit Team target", "", func(a *app.Arguments, s string) { a.TeamID = s }),
		ui.TextField("Member ID", "explicit Team member", "", func(a *app.Arguments, s string) { a.MemberID = s }),
		ui.TextField("Non-Git project root", "optional explicit project root", "", func(a *app.Arguments, s string) { a.ProjectRoot = s }),
	}
}

func scheduleHookFields() []ui.Field {
	return []ui.Field{
		ui.TextField("Failure hook", "opt-in absolute installed executable", "", func(a *app.Arguments, s string) {
			hook := ui.Defaults("schedule", "create").ScheduleHook
			if a.ScheduleHook != nil {
				copy := *a.ScheduleHook
				hook = &copy
			}
			hook.Executable = s
			a.ScheduleHook = hook
		}),
		ui.IntegerField("Failure threshold", 1, func(a *app.Arguments, n int64) { a.ScheduleHook.Threshold = n }),
		{Label: "Hook timeout", Hint: "positive duration, at most 1m", Value: "10s", Apply: func(a *app.Arguments, s string) error {
			value, err := time.ParseDuration(s)
			if err == nil {
				a.ScheduleHook.Timeout = value
			}
			return err
		}},
	}
}

func (v views) inspectExecution() ui.Action {
	a := v.defaults("schedule", "execution")
	return ui.Action{ID: "schedule-execution", Label: "Inspect execution", Arguments: a, Kind: session.Read, Fields: []ui.Field{ui.TextField("Task ID", "scheduled occurrence task ID", "", func(a *app.Arguments, s string) { a.TaskID = s })}, Run: v.runSchedule, Reconciliation: func(a app.Arguments) []string { return scopedReadback(a, "schedule", "execution", a.TaskID) }}
}

func (v views) runSchedule(ctx context.Context, a app.Arguments) ui.Feedback {
	var result app.OperationResult
	if scheduleMutation(a) {
		result = v.backend.ScheduleMutation(a)
	} else {
		result = v.backend.Schedules(ctx, a)
	}
	feedback := ui.Result(result)
	feedback.Title = "Schedule " + strings.TrimSpace(a.ScheduleAction+" "+a.SchedulePolicyOperation)
	if item := result.Response.Schedule; item != nil {
		feedback.Body = fmt.Sprintf("Schedule: %s\nState: %s\nSaved: %t\nNext: %s\n", item.ID, item.State, item.Saved, item.NextAction)
	}
	if h := result.Response.ScheduleHealth; h != nil {
		feedback.Body = fmt.Sprintf("Schedule: %s\nConsecutive non-committed: %d\nDominant failure: %s\nCoverage complete: %t\nUnresolved: %d\n", h.ScheduleID, h.ConsecutiveNonCommitted, h.DominantFailureCode, h.Complete, h.Unresolved)
		if h.LastCommitted != nil {
			feedback.Body += fmt.Sprintf("Last committed: %s (due %s)\n", h.LastCommitted.TaskID, h.LastCommitted.Due.Format(time.RFC3339))
		}
		if h.Hold != nil {
			feedback.Body += fmt.Sprintf("Quota hold: until %s\n", h.Hold.Until.Format(time.RFC3339))
		}
	}
	if p := result.Response.SchedulePolicy; p != nil {
		feedback.Body = fmt.Sprintf("Quota hold enabled: %t\nMaximum hold: %s\nRevision: %d\n", p.Enabled, p.MaxHold, p.Revision)
	}
	if a.ScheduleAction == "show" || a.ScheduleAction == "history" || a.ScheduleAction == "execution" {
		encoded, encodeErr := json.MarshalIndent(result.Response, "", "  ")
		if encodeErr != nil {
			feedback.Err = encodeErr
			return feedback
		}
		feedback.Body = string(encoded)
	}
	return feedback
}

func scheduleReadback(a app.Arguments) []string {
	return scopedReadback(a, "schedule", "show", a.ScheduleID)
}

func scheduleMutation(a app.Arguments) bool {
	switch a.ScheduleAction {
	case "create", "pause", "resume", "delete":
		return true
	case "policy":
		return a.SchedulePolicyOperation == "set"
	default:
		return false
	}
}
