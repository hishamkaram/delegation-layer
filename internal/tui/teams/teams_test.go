package teams

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

type backendProbe struct {
	teamsResponse app.TeamsResponse
	requested     []app.Arguments
	inputs        []string
	failure       error
}

func (b *backendProbe) Teams(_ context.Context, a app.Arguments, input io.Reader) (app.TeamsResponse, error) {
	b.requested = append(b.requested, a)
	if input != nil {
		data, err := io.ReadAll(input)
		if err != nil {
			return app.TeamsResponse{}, err
		}
		b.inputs = append(b.inputs, string(data))
	}
	return b.teamsResponse, b.failure
}

func TestTeamPagePreservesScopeRevisionAndConflict(t *testing.T) {
	rows := []app.TeamsListRow{{Reference: task.TeamReference{Scope: "project", TeamID: "reviewers"}, Revision: 7, Name: "Reviewers", Purpose: "Review code", MemberCount: 2}}
	probe := &backendProbe{teamsResponse: app.TeamsResponse{Teams: &rows, NextCursor: "project:reviewers"}}
	section := NewSection(probe, app.Arguments{Root: "/state"}, "/project")
	page, err := section.Load(t.Context(), ui.PageRequest{Search: "review", After: "user:previous"})
	if err != nil || len(page.Entries) != 1 || page.Next != "project:reviewers" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if probe.requested[0].TeamAfter != "user:previous" || probe.requested[0].Cwd != "/project" {
		t.Fatalf("list context lost: %+v", probe.requested[0])
	}
	update := page.Entries[0].Actions[1]
	if update.Kind != session.Effect || !update.Confirm {
		t.Fatal("update has no explicit confirmation/effect ownership")
	}
	request, err := update.Prepare([]string{"/definition.json", ""})
	if err != nil {
		t.Fatal(err)
	}
	conflict := errors.New("catalog revision changed; reload")
	probe.failure = conflict
	feedback := update.Run(t.Context(), request)
	if !errors.Is(feedback.Err, conflict) || len(probe.requested) != 2 {
		t.Fatalf("conflict lost or retried: %+v calls%d", feedback, len(probe.requested))
	}
	sent := probe.requested[1]
	if sent.TeamExpectedRevision != 7 || sent.TeamScope != "project" || sent.TeamID != "reviewers" || sent.Root != "/state" {
		t.Fatalf("target changed: %+v", sent)
	}
}

func TestTeamInlineDefinitionAndConflictingSources(t *testing.T) {
	probe := &backendProbe{}
	section := NewSection(probe, app.Arguments{Root: "/state"}, "/project")
	page, err := section.Load(t.Context(), ui.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	probe.requested = nil
	action := actionByID(t, section, page, "team-register")
	values := fieldDefaults(action.Fields)
	values[4] = `{"schema_version":1,"id":"reviewers","name":"Reviewers","purpose":"Review","roles":[],"members":[]}`
	a, err := action.Prepare(values)
	if err != nil {
		t.Fatal(err)
	}
	feedback := action.Run(t.Context(), a)
	if feedback.Err != nil || len(probe.inputs) != 1 || probe.inputs[0] != values[4] {
		t.Fatalf("inline definition changed: %+v %q", feedback, probe.inputs)
	}
	a.TeamDefinition = "/other.json"
	feedback = action.Run(t.Context(), a)
	if feedback.Err == nil || len(probe.requested) != 1 {
		t.Fatal("conflicting sources reached the backend")
	}
}

func fieldDefaults(fields []ui.Field) []string {
	result := make([]string, len(fields))
	for i, f := range fields {
		result[i] = f.Value
	}
	return result
}
