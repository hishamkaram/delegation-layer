package teams

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/testutil/goroutinecheck"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func TestMain(m *testing.M) {
	if code, handled := execution.RunCommandOwnerMode(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(goroutinecheck.Run(m))
}

func TestGoroutineLeakGuard(t *testing.T) { goroutinecheck.VerifyInstalled(t) }

func actionByID(t *testing.T, section ui.Section, page ui.Page, id string) ui.Action {
	t.Helper()
	for _, action := range slices.Concat(section.Actions, page.Actions) {
		if action.ID == id {
			return action
		}
	}
	t.Fatalf("action %s unavailable", id)
	return ui.Action{}
}

func preparedAction(t *testing.T, action ui.Action, edits map[string]string) app.Arguments {
	t.Helper()
	values := fieldDefaults(action.Fields)
	for i, field := range action.Fields {
		if value, ok := edits[field.Label]; ok {
			values[i] = value
			delete(edits, field.Label)
		}
	}
	if len(edits) > 0 {
		t.Fatalf("unknown form fields: %v", edits)
	}
	a, err := action.Prepare(values)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNonGitProjectTeamRemainsManageableAcrossContextSwitch(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("native Git required:", err)
	}
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := app.Dependencies{TeamCatalogPath: filepath.Join(t.TempDir(), "catalog", "teams.sqlite"), Environment: []string{"PATH=" + filepath.Dir(git), "HOME=" + t.TempDir(), "GIT_CEILING_DIRECTORIES=" + filepath.Dir(project), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}}
	section := NewSection(app.NewWorkbench(deps), app.Arguments{Root: t.TempDir()}, project)
	page, err := section.Load(t.Context(), ui.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	register := actionByID(t, section, page, "team-register")
	request := preparedAction(t, register, map[string]string{"Scope": "project", "Project directory": project, "Non-Git project root": project, "Definition JSON": `{"schema_version":1,"id":"review","name":"Review","purpose":"Review code","roles":[],"members":[]}`})
	if feedback := register.Run(t.Context(), request); feedback.Err != nil {
		t.Fatal(feedback.Err)
	}
	selectProject := actionByID(t, section, page, "team-project-context")
	selectRequest := preparedAction(t, selectProject, map[string]string{"Project directory": project, "Non-Git project root": project})
	if feedback := selectProject.Run(t.Context(), selectRequest); feedback.Err != nil {
		t.Fatal(feedback.Err)
	}
	page, err = section.Load(t.Context(), ui.PageRequest{})
	if err != nil || len(page.Entries) != 1 {
		t.Fatalf("registered non-Git Team inaccessible: %+v %v", page, err)
	}
	assertFrozenProjectActions(t, section, page, selectProject, project, other)
}

func assertFrozenProjectActions(t *testing.T, section ui.Section, page ui.Page, selectProject ui.Action, project, other string) {
	t.Helper()
	show, update := page.Entries[0].Actions[0], page.Entries[0].Actions[1]
	request := preparedAction(t, update, map[string]string{"Definition JSON": `{"schema_version":1,"id":"review","name":"Updated","purpose":"Review code","roles":[],"members":[]}`})
	selection := preparedAction(t, selectProject, map[string]string{"Project directory": other, "Non-Git project root": other})
	if feedback := selectProject.Run(t.Context(), selection); feedback.Err != nil {
		t.Fatal(feedback.Err)
	}
	otherPage, err := section.Load(t.Context(), ui.PageRequest{})
	if err != nil || len(otherPage.Entries) != 0 {
		t.Fatalf("selected project not applied: %+v %v", otherPage, err)
	}
	if feedback := show.Run(t.Context(), show.Arguments); feedback.Err != nil || !strings.Contains(feedback.Body, `"Review"`) {
		t.Fatalf("frozen show lost old context: %+v", feedback)
	}
	if feedback := update.Run(t.Context(), request); feedback.Err != nil {
		t.Fatalf("frozen update lost old context: %+v", feedback)
	}
	if feedback := update.Run(t.Context(), request); feedback.Err == nil {
		t.Fatal("stale revision unexpectedly overwrote Team")
	}
	readback := update.Reconciliation(request)
	if request.Cwd != project || request.ProjectRoot != project || !slices.Contains(readback, project) || slices.Contains(readback, other) {
		t.Fatalf("row action retargeted after context switch: %+v %v", request, readback)
	}
}

func TestTeamContextRejectsPartialProjectValidation(t *testing.T) {
	probe := &backendProbe{teamsResponse: app.TeamsResponse{Diagnostics: []app.TeamsDiagnostic{{Code: "project_context", Message: "project directory is outside the explicit root"}}}}
	section := NewSection(probe, app.Arguments{Root: "/state"}, "/original")
	page, err := section.Load(t.Context(), ui.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	selectProject := actionByID(t, section, page, "team-project-context")
	request := preparedAction(t, selectProject, map[string]string{"Project directory": "/different", "Non-Git project root": "/root"})
	feedback := selectProject.Run(t.Context(), request)
	if feedback.Err == nil || feedback.ResetPage {
		t.Fatalf("partial list accepted as validated context: %+v", feedback)
	}
	if _, err := section.Load(t.Context(), ui.PageRequest{}); err != nil {
		t.Fatal(err)
	}
	last := probe.requested[len(probe.requested)-1]
	if last.Cwd != "/original" || last.ProjectRoot != "" {
		t.Fatalf("failed selection was published: %+v", last)
	}
}

type contextBackend func(context.Context, app.Arguments, io.Reader) (app.TeamsResponse, error)

func (f contextBackend) Teams(ctx context.Context, a app.Arguments, input io.Reader) (app.TeamsResponse, error) {
	return f(ctx, a, input)
}

func TestTeamContextDiscardsLoadStartedBeforeSelection(t *testing.T) {
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	backend := contextBackend(func(ctx context.Context, a app.Arguments, _ io.Reader) (app.TeamsResponse, error) {
		if a.Cwd == "/original" && calls.Add(1) > 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return app.TeamsResponse{}, ctx.Err()
			}
		}
		return app.TeamsResponse{}, nil
	})
	section := NewSection(backend, app.Arguments{}, "/original")
	page, err := section.Load(ctx, ui.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, err := section.Load(ctx, ui.PageRequest{}); done <- err }()
	defer func() { cancel(); <-done }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	action := actionByID(t, section, page, "team-project-context")
	request := preparedAction(t, action, map[string]string{"Project directory": "/selected"})
	feedback := action.Run(ctx, request)
	if feedback.Err != nil || !feedback.ResetPage {
		t.Fatalf("context not applied: %+v", feedback)
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("old context load published: %v", err)
		}
		done <- err
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestTeamProjectSelectorReopensCurrentContext(t *testing.T) {
	section := NewSection(&backendProbe{}, app.Arguments{}, "/initial")
	page, err := section.Load(t.Context(), ui.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	action := actionByID(t, section, page, "team-project-context")
	request := preparedAction(t, action, map[string]string{"Project directory": "/selected", "Non-Git project root": "/selected-root"})
	if feedback := action.Run(t.Context(), request); feedback.Err != nil {
		t.Fatal(feedback.Err)
	}
	page, err = section.Load(t.Context(), ui.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	reopened := actionByID(t, section, page, "team-project-context")
	defaults := preparedAction(t, reopened, nil)
	if defaults.Cwd != "/selected" || defaults.ProjectRoot != "/selected-root" {
		t.Fatalf("selector reopened stale context: %+v", defaults)
	}
}
