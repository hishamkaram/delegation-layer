package teams

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func (v views) loadTeams(ctx context.Context, page ui.PageRequest) (ui.Page, error) {
	a := v.defaults("teams", "list")
	selected := v.project.snapshot()
	a.Cwd, a.ProjectRoot = selected.cwd, selected.root
	a.TeamAfter = page.After
	response, err := v.backend.Teams(ctx, a, nil)
	if err != nil {
		return ui.Page{}, err
	}
	if v.project.snapshot().version != selected.version {
		return ui.Page{}, context.Canceled
	}
	result := ui.Page{Next: response.NextCursor, Actions: []ui.Action{v.selectProject(), v.registerTeam(selected), v.defaultTeam("show", selected), v.defaultTeam("set", selected), v.defaultTeam("clear", selected)}, Summary: "Optional scoped configuration; search filters this page. Defaults never auto-route tasks."}
	result.Summary += "\nTeam project: " + selected.cwd
	if selected.root != "" {
		result.Summary += "\nNon-Git root: " + selected.root
	}
	if response.Teams == nil {
		return result, nil
	}
	for _, row := range *response.Teams {
		if !matching(page.Search, row.Reference.TeamID, row.Name, row.Purpose, row.Reference.Scope) {
			continue
		}
		result.Entries = append(result.Entries, ui.Entry{
			ID: row.Reference.Scope + ":" + row.Reference.TeamID, Title: row.Name,
			Subtitle: fmt.Sprintf("%s • revision %d • %d members", row.Reference.Scope, row.Revision, row.MemberCount),
			Detail:   fmt.Sprintf("Team: %s\nScope: %s\nRevision: %d\n%s\n\nProvider readiness is not implied by a Team definition.", row.Reference.TeamID, row.Reference.Scope, row.Revision, row.Purpose),
			Actions:  []ui.Action{v.teamAction("show", row, selected), v.teamAction("update", row, selected), v.teamAction("delete", row, selected)},
		})
	}
	for _, diagnostic := range response.Diagnostics {
		result.Summary += "\n" + diagnostic.Code + ": " + diagnostic.Message
	}
	return result, nil
}

func (v views) registerTeam(selected projectSnapshot) ui.Action {
	a := v.defaults("teams", "register")
	scope, cwd, root := "user", "", ""
	if selected.version > 0 {
		scope, cwd, root = "project", selected.cwd, selected.root
	}
	return ui.Action{
		ID: "team-register", Label: "Register Team", Description: "Create a scoped Team from a complete definition. This does not check provider readiness.", Arguments: a, Kind: session.Effect, Confirm: true,
		Fields:         append(teamContextFields(scope, cwd, root), definitionFields()...),
		Reconciliation: teamReadback, Run: v.runTeam,
	}
}

func (v views) teamAction(action string, row app.TeamsListRow, selected projectSnapshot) ui.Action {
	a := v.defaults("teams", action)
	a.TeamScope = row.Reference.Scope
	a.TeamID = row.Reference.TeamID
	a.TeamExpectedRevision = row.Revision
	if row.Reference.Scope == "project" {
		a.Cwd, a.ProjectRoot = selected.cwd, selected.root
	}
	result := ui.Action{ID: "team-" + action, Label: strings.ToUpper(action[:1]) + action[1:] + " Team", Arguments: a, Kind: session.Read, Run: v.runTeam, Reconciliation: teamReadback}
	if action == "show" {
		return result
	}
	result.Kind = session.Effect
	result.Confirm = true
	result.Description = fmt.Sprintf("%s %s at expected revision %d. A conflict requires reload and review; no overwrite or retry.", action, row.Reference.TeamID, row.Revision)
	if action == "update" {
		result.Fields = definitionFields()
	}
	return result
}

func teamContextFields(scope, cwd, root string) []ui.Field {
	return []ui.Field{
		ui.TextField("Scope", "user or project", scope, func(a *app.Arguments, s string) { a.TeamScope = s }),
		ui.TextField("Project directory", "absolute; required for project scope only", cwd, func(a *app.Arguments, s string) { a.Cwd = s }),
		ui.TextField("Non-Git project root", "absolute; only when explicitly using a non-Git project", root, func(a *app.Arguments, s string) { a.ProjectRoot = s }),
	}
}

func definitionFields() []ui.Field {
	inline := ui.TextField("Definition JSON", "paste a complete Team definition; choose text or a file, not both", "", func(a *app.Arguments, s string) {
		if s != "" {
			a.TeamDefinitionBytes = []byte(s)
		}
	})
	inline.Multiline = true
	return []ui.Field{
		ui.TextField("Definition file", "existing JSON file; leave empty when pasting JSON", "", func(a *app.Arguments, s string) { a.TeamDefinition = s }), inline,
	}
}

func (v views) runTeam(ctx context.Context, a app.Arguments) ui.Feedback {
	var input io.Reader
	if a.TeamDefinitionBytes != nil {
		if a.TeamDefinition != "" {
			return ui.Feedback{Title: "Team", Err: fmt.Errorf("choose a definition file or inline JSON, not both")}
		}
		input = strings.NewReader(string(a.TeamDefinitionBytes))
	}
	response, err := v.backend.Teams(ctx, a, input)
	var body strings.Builder
	fmt.Fprintf(&body, "Team %s: %s\n", a.TeamAction, response.Status)
	if m := response.Mutation; m != nil {
		fmt.Fprintf(&body, "Operation: %s\nRevision: %d\n", m.Operation, m.Revision)
		if m.Reference != nil {
			fmt.Fprintf(&body, "Scope: %s\nTeam: %s\n", m.Reference.Scope, m.Reference.TeamID)
		}
	}
	if d := response.Default; d != nil {
		fmt.Fprintf(&body, "Project default: %s\n", d.State)
		if d.Reference != nil {
			fmt.Fprintf(&body, "%s / %s\n", d.Reference.Scope, d.Reference.TeamID)
		}
	}
	if response.Team != nil {
		encoded, encodeErr := json.MarshalIndent(response.Team, "", "  ")
		if encodeErr != nil {
			return ui.Feedback{Title: "Team", Err: encodeErr}
		}
		body.Write(encoded)
	}
	if response.Failure != nil {
		fmt.Fprintf(&body, "Failure: %s\nNext: %s\n", response.Failure.Code, response.Failure.NextAction)
	}
	return ui.Feedback{Title: "Team " + a.TeamAction, Body: body.String(), Err: err}
}

func teamReadback(a app.Arguments) []string {
	if a.TeamAction == "default" {
		result := scopedReadback(a, "teams", "default", "show", "--cwd", a.Cwd)
		if a.ProjectRoot != "" {
			result = append(result, "--project-root", a.ProjectRoot)
		}
		return result
	}
	result := scopedReadback(a, "teams", "show", "--scope", a.TeamScope, "--team", a.TeamID)
	if a.TeamAction == "register" && a.TeamID == "" {
		result = scopedReadback(a, "teams", "list")
	}
	if a.Cwd != "" {
		result = append(result, "--cwd", a.Cwd)
	}
	if a.ProjectRoot != "" {
		result = append(result, "--project-root", a.ProjectRoot)
	}
	return result
}

func (v views) defaultTeam(operation string, selected projectSnapshot) ui.Action {
	a := v.defaults("teams", "default", operation)
	a.Cwd, a.ProjectRoot = selected.cwd, selected.root
	fields := projectFields(selected.cwd, selected.root)
	kind := session.Read
	switch operation {
	case "set":
		kind = session.Effect
		fields = append(fields, ui.TextField("Team scope", "user or project", "user", func(a *app.Arguments, s string) { a.TeamScope = s }), ui.TextField("Team ID", "existing Team ID", "", func(a *app.Arguments, s string) { a.TeamID = s }))
	case "clear":
		kind = session.Effect
	}
	return ui.Action{ID: "team-default-" + operation, Label: "Project default: " + operation, Description: "Defaults guide explicit selection; they do not automatically route tasks.", Fields: fields, Arguments: a, Kind: kind, Confirm: kind == session.Effect, Run: v.runTeam, Reconciliation: teamReadback}
}
