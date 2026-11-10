package workspace

import (
	"context"
	"slices"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
	workspacefixture "github.com/hishamkaram/delegation-layer/internal/workspaces/testsupport"
)

type backendProbe struct{ requests []app.Arguments }

func (p *backendProbe) Workspace(_ context.Context, a app.Arguments) app.OperationResult {
	p.requests = append(p.requests, a)
	return app.OperationResult{}
}

func TestExecPreservesArgvAndNeedsConfirmation(t *testing.T) {
	probe := &backendProbe{}
	for _, action := range Actions(probe, app.Arguments{Root: "/state"}) {
		if action.ID != "workspace-exec" {
			continue
		}
		if !action.Confirm || action.Kind != session.Effect {
			t.Fatal("exec is not a confirmed tracked operation")
		}
		request, err := action.Prepare(execValues(action.Fields))
		if err != nil {
			t.Fatal(err)
		}
		action.Run(t.Context(), request)
		if len(probe.requests) != 1 || strings.Join(probe.requests[0].ExecArguments, "|") != "/installed/program|quoted ' value|--help|" {
			t.Fatalf("argv was shell-parsed or trimmed: %+v", probe.requests)
		}
		if probe.requests[0].Root != "/state" {
			t.Fatal("root changed")
		}
		return
	}
	t.Fatal("exec action missing")
}

func execValues(fields []ui.Field) []string {
	values := make([]string, len(fields))
	for i, f := range fields {
		values[i] = f.Value
		switch f.Label {
		case "Workspace ID":
			values[i] = strings.Repeat("a", 32)
		case "Command argv":
			values[i] = "/installed/program\nquoted ' value\n--help\n\n"
		}
	}
	return values
}

func TestEmptyArgvAndNULRejectedBeforeExecution(t *testing.T) {
	for _, value := range []string{"", "\nargument", "command\ncontains\x00NUL"} {
		if err := applyArgv(&app.Arguments{}, value); err == nil {
			t.Errorf("invalid argv %q accepted", value)
		}
	}
}

func TestArgvEditorUsesOneFinalNewlineAsDelimiter(t *testing.T) {
	for _, fixture := range []struct {
		text string
		want []string
	}{
		{"program\nvalue\n", []string{"program", "value"}},
		{"program\nvalue\n\n", []string{"program", "value", ""}},
		{"program\n\nvalue", []string{"program", "", "value"}},
		{"program\n value ", []string{"program", " value "}},
	} {
		editor := textarea.New()
		editor.SetValue(fixture.text)
		if editor.Value() != fixture.text {
			t.Fatalf("editor changed input: %q", editor.Value())
		}
		var request app.Arguments
		if err := applyArgv(&request, editor.Value()); err != nil || !slices.Equal(request.ExecArguments, fixture.want) {
			t.Fatalf("newline convention changed argv: %q -> %q want %q; %v", fixture.text, request.ExecArguments, fixture.want, err)
		}
	}
}

func TestReleaseFormPreservesBusyWorkspaceRefusal(t *testing.T) {
	store, resource := workspacefixture.NewResource(t)
	owner, err := resource.AcquireUseOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if releaseErr := owner.Release(); releaseErr != nil {
			t.Error(releaseErr)
		}
	})
	backend := app.NewWorkbench(app.Dependencies{Environment: []string{}})
	for _, action := range Actions(backend, app.Arguments{Root: store.Root}) {
		if action.ID != "workspace-release" {
			continue
		}
		values := make([]string, len(action.Fields))
		for i, field := range action.Fields {
			values[i] = field.Value
			if field.Label == "Workspace ID" {
				values[i] = resource.Request().WorkspaceID
			}
		}
		request, prepareErr := action.Prepare(values)
		if prepareErr != nil {
			t.Fatal(prepareErr)
		}
		if !action.Confirm || action.Kind != session.Effect || request.Root != store.Root {
			t.Fatalf("release lost exact confirmation: %+v", action)
		}
		feedback := action.Run(t.Context(), request)
		if feedback.Err != nil || !strings.Contains(feedback.Body, "requested_action_not_authorized") || !strings.Contains(feedback.Body, "retry_release") {
			t.Fatalf("busy release was acknowledged or silently retried: %+v", feedback)
		}
		return
	}
	t.Fatal("workspace release action missing")
}
