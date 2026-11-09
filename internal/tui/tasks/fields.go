package tasks

import (
	"strconv"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func textField(label, hint, value string, apply func(*app.Arguments, string)) ui.Field {
	return ui.Field{Label: label, Hint: hint, Value: value, Apply: func(a *app.Arguments, value string) error { apply(a, value); return nil }}
}

func booleanField(label, hint string, value bool, apply func(*app.Arguments, bool)) ui.Field {
	return ui.Field{Label: label, Hint: hint, Value: strconv.FormatBool(value), Apply: func(a *app.Arguments, text string) error {
		parsed, err := strconv.ParseBool(text)
		if err != nil {
			return err
		}
		apply(a, parsed)
		return nil
	}}
}

func durationField(label string, value time.Duration, apply func(*app.Arguments, time.Duration)) ui.Field {
	return ui.Field{Label: label, Hint: "finite duration; owner validates supported limits", Value: value.String(), Apply: func(a *app.Arguments, text string) error {
		parsed, err := time.ParseDuration(text)
		if err != nil {
			return err
		}
		apply(a, parsed)
		return nil
	}}
}

func briefFields() []ui.Field {
	return []ui.Field{
		textField("Brief file", "existing file; choose this or inline text", "", func(a *app.Arguments, value string) { a.Brief = value }),
		{Label: "Brief text", Hint: "choose inline text or a file; leave both blank only to reuse a continuation brief", Multiline: true, Apply: func(a *app.Arguments, value string) error {
			if value != "" {
				a.BriefBytes = []byte(value)
			}
			return nil
		}},
	}
}

func advanced(fields ...ui.Field) []ui.Field {
	for i := range fields {
		fields[i].Advanced = true
	}
	return fields
}

func executionFields(defaults app.Arguments) []ui.Field {
	primary := []ui.Field{
		textField("Permission", "native permission intent, not an independent sandbox", defaults.Config.Permission, func(a *app.Arguments, s string) { a.Config.Permission = s }),
		textField("Budget", "existing task budget duration", defaults.Config.Budget, func(a *app.Arguments, s string) { a.Config.Budget = s }),
	}
	primary = append(primary, advanced(
		textField("Requested model", "exact native ID; discovery is advisory", defaults.Config.Model, func(a *app.Arguments, s string) { a.Config.Model = s }),
		textField("Effort", "exact requested effort; blank keeps owner default", defaults.Config.Effort, func(a *app.Arguments, s string) { a.Config.Effort = s }),
		textField("Output silence timeout", "opt-in; blank keeps default disabled", defaults.Config.OutputSilenceTimeout, func(a *app.Arguments, s string) { a.Config.OutputSilenceTimeout = s }),
		textField("Native timeout", "optional provider-native timeout", defaults.Config.NativeTimeout, func(a *app.Arguments, s string) { a.Config.NativeTimeout = s }),
		booleanField("Interactive", "true enables supported native clarification", defaults.Config.Interactive, func(a *app.Arguments, b bool) { a.Config.Interactive = b }),
		textField("Review target", "optional uncommitted, base:BRANCH or commit:SHA", defaults.ReviewTarget, func(a *app.Arguments, s string) { a.ReviewTarget = s }),
	)...)
	return append(primary, advanced(resourceGuardFields(defaults)...)...)
}

func teamFields() []ui.Field {
	return []ui.Field{
		textField("Team scope", "optional user or project; requires explicit Team/member", "", func(a *app.Arguments, s string) { a.TeamScope = s }),
		textField("Team ID", "explicit optional Team", "", func(a *app.Arguments, s string) { a.TeamID = s }),
		textField("Member ID", "explicit optional Member", "", func(a *app.Arguments, s string) { a.MemberID = s }),
		textField("Non-Git project root", "optional absolute project identity", "", func(a *app.Arguments, s string) { a.ProjectRoot = s }),
	}
}

func readback(a app.Arguments, command string, handle string) []string {
	return []string{"delegate", "--root", a.Root, command, handle, "--json"}
}

func taskIDField() ui.Field {
	return textField("Task ID", "exact local ID or origin-recorded remote:ID", "", func(a *app.Arguments, s string) { a.TaskID = strings.TrimSpace(s) })
}

func exactIDAction(action ui.Action, id string) ui.Action {
	if id == "" {
		action.Label += " by ID"
		action.Fields = append([]ui.Field{taskIDField()}, action.Fields...)
	}
	return action
}

func resourceGuardFields(defaults app.Arguments) []ui.Field {
	return []ui.Field{
		booleanField("CPU guard", "on by default; false disables for this admission only", !defaults.ResourceGuards.DisableCPU, func(a *app.Arguments, b bool) { a.ResourceGuards.DisableCPU = !b }),
		booleanField("Memory guard", "on by default; false disables for this admission only", !defaults.ResourceGuards.DisableMemory, func(a *app.Arguments, b bool) { a.ResourceGuards.DisableMemory = !b }),
	}
}
