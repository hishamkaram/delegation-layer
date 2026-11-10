package schedules

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

type backendProbe struct {
	scheduleResponse app.OperationResult
	requested        []app.Arguments
	reads, mutations int
}

func (b *backendProbe) Schedules(_ context.Context, a app.Arguments) app.OperationResult {
	b.requested = append(b.requested, a)
	b.reads++
	return b.scheduleResponse
}

func (b *backendProbe) ScheduleMutation(a app.Arguments) app.OperationResult {
	b.requested = append(b.requested, a)
	b.mutations++
	return b.scheduleResponse
}

func TestScheduleReadsStaySeparateFromMutations(t *testing.T) {
	rows := []app.ScheduleListRow{{ID: strings.Repeat("a", 32), State: "enabled", Cron: "0 */2 * * *", Timezone: "Asia/Bangkok", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
	probe := &backendProbe{scheduleResponse: app.OperationResult{Response: app.Response{Schedules: &rows, ScheduleCursor: "next"}}}
	section := NewSection(probe, app.Arguments{Root: "/state"}, "/project")
	page, err := section.Load(t.Context(), ui.PageRequest{After: "cursor"})
	if err != nil || len(page.Entries) != 1 || probe.reads != 1 || probe.mutations != 0 {
		t.Fatalf("refresh mutated state: %+v %v", page, err)
	}
	if probe.requested[0].ScheduleAfter != "cursor" {
		t.Fatal("page cursor was lost")
	}
	for _, action := range page.Entries[0].Actions {
		if action.Arguments.ScheduleAction == "pause" {
			if !action.Confirm || action.Kind != session.Effect {
				t.Fatal("pause is not confirmed effect")
			}
			action.Run(t.Context(), action.Arguments)
			if probe.mutations != 1 {
				t.Fatal("pause bypassed mutation owner")
			}
		}
	}
}

func TestScheduleFormPreservesRequestAndOwnsHookCopy(t *testing.T) {
	probe := &backendProbe{}
	action := NewSection(probe, app.Arguments{Root: "/state"}, "/project").Actions[0]
	values := scheduleValues(action.Fields)
	a, err := action.Prepare(values)
	if err != nil {
		t.Fatal(err)
	}
	if err = task.ValidateTaskID(a.ScheduleID); err != nil {
		t.Fatal(err)
	}
	if a.Root != "/state" || a.Provider != "claude:print" || a.Config.Model != "requested-model" || a.Config.Permission != "workspace-write" || string(a.BriefBytes) != "exact brief" || !a.ScheduleTimezoneSet {
		t.Fatalf("request changed: %+v", a)
	}
	if action.Arguments.ScheduleHook.Executable != "" {
		t.Fatal("form mutated shared registration defaults")
	}
	second, err := action.Prepare(fieldDefaults(action.Fields))
	if err != nil {
		t.Fatal(err)
	}
	if second.ScheduleHook == a.ScheduleHook || a.ScheduleHook.Executable != "/installed/hook" {
		t.Fatal("prepared requests share mutable hook state")
	}
	action.Run(t.Context(), a)
	if probe.mutations != 1 || probe.reads != 0 {
		t.Fatal("create did not use mutation owner exclusively")
	}
}

func fieldDefaults(fields []ui.Field) []string {
	result := make([]string, len(fields))
	for i, f := range fields {
		result[i] = f.Value
	}
	return result
}

func scheduleValues(fields []ui.Field) []string {
	values := fieldDefaults(fields)
	for i, field := range fields {
		switch field.Label {
		case "Cron":
			values[i] = "0 */2 * * *"
		case "Provider":
			values[i] = "claude:print"
		case "Brief text":
			values[i] = "exact brief"
		case "Timezone":
			values[i] = "Asia/Bangkok"
		case "Model":
			values[i] = "requested-model"
		case "Permission":
			values[i] = "workspace-write"
		case "Failure hook":
			values[i] = "/installed/hook"
		}
	}
	return values
}
