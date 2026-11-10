// Package schedules presents immutable schedules and scheduler-owned policy.
package schedules

import (
	"context"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

type (
	Backend interface {
		Schedules(context.Context, app.Arguments) app.OperationResult
		ScheduleMutation(app.Arguments) app.OperationResult
	}
	views struct {
		backend Backend
		base    app.Arguments
		cwd     string
	}
)

func NewSection(backend Backend, base app.Arguments, cwd string) ui.Section {
	v := views{backend: backend, base: base, cwd: cwd}
	return ui.Section{Name: "Schedules", Load: v.loadSchedules, Actions: []ui.Action{v.createSchedule(), v.inspectExecution()}}
}

func (v views) defaults(path ...string) app.Arguments {
	a := ui.Defaults(path...)
	a.Root = v.base.Root
	return a
}

func matching(search string, fields ...string) bool {
	return search == "" || strings.Contains(strings.ToLower(strings.Join(fields, " ")), strings.ToLower(search))
}

func scopedReadback(a app.Arguments, command ...string) []string {
	result := []string{"delegate", "--root", a.Root}
	return append(result, command...)
}
