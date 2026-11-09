// Package teams presents scoped Team configuration through its catalog owner.
package teams

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

type (
	Backend interface {
		Teams(context.Context, app.Arguments, io.Reader) (app.TeamsResponse, error)
	}
	views struct {
		backend Backend
		base    app.Arguments
		project *projectContext
	}
)

func NewSection(backend Backend, base app.Arguments, cwd string) ui.Section {
	v := views{backend: backend, base: base, project: &projectContext{cwd: cwd}}
	return ui.Section{Name: "Teams", Load: v.loadTeams}
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

// projectContext is owned by this Team view session. Only a successfully
// validated Select action publishes it; loads read immutable snapshots and
// freeze both paths into row actions. The mutex also protects context changes
// against background loads; no persisted or execution-root state lives here.
type projectContext struct {
	mu        sync.RWMutex
	cwd, root string
	version   uint64
}

type projectSnapshot struct {
	cwd, root string
	version   uint64
}

func (p *projectContext) snapshot() projectSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return projectSnapshot{p.cwd, p.root, p.version}
}

func (p *projectContext) publish(cwd, root string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cwd, p.root = cwd, root
	p.version++
}

func (v views) selectProject() ui.Action {
	selected := v.project.snapshot()
	a := v.defaults("teams", "list")
	a.Cwd, a.ProjectRoot = selected.cwd, selected.root
	return ui.Action{
		ID: "team-project-context", Label: "Select Team project", Description: "Validate an explicit project context for Team listing and new actions. Existing row actions keep their original project.", Arguments: a, Kind: session.Read,
		Fields: projectFields(selected.cwd, selected.root), Run: func(ctx context.Context, a app.Arguments) ui.Feedback {
			if a.Cwd == "" {
				return ui.Feedback{Err: fmt.Errorf("choose an explicit project directory")}
			}
			response, err := v.backend.Teams(ctx, a, nil)
			if err != nil {
				return ui.Feedback{Err: err}
			}
			for _, diagnostic := range response.Diagnostics {
				if diagnostic.Code == "project_context" {
					return ui.Feedback{Err: fmt.Errorf("%s", diagnostic.Message)}
				}
			}
			if err := ctx.Err(); err != nil {
				return ui.Feedback{Err: err}
			}
			v.project.publish(a.Cwd, a.ProjectRoot)
			return ui.Feedback{Title: "Team project selected", Body: a.Cwd, ResetPage: true}
		},
	}
}

func projectFields(cwd, root string) []ui.Field {
	return []ui.Field{
		ui.TextField("Project directory", "explicit absolute project context", cwd, func(a *app.Arguments, s string) { a.Cwd = s }),
		ui.TextField("Non-Git project root", "absolute containing root outside Git; empty for Git", root, func(a *app.Arguments, s string) { a.ProjectRoot = s }),
	}
}
