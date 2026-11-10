package checkpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
)

type backendProbe struct{ requests []app.Arguments }

func (p *backendProbe) Checkpoint(_ context.Context, a app.Arguments) app.OperationResult {
	p.requests = append(p.requests, a)
	return app.OperationResult{}
}

func TestCreationKeepsUnknownIdentityAndExplicitTarget(t *testing.T) {
	probe := &backendProbe{}
	action := Actions(probe, app.Arguments{Root: "/state"}, "/project")[0]
	values := make([]string, len(action.Fields))
	for i, f := range action.Fields {
		values[i] = f.Value
	}
	request, err := action.Prepare(values)
	if err != nil {
		t.Fatal(err)
	}
	if request.CheckpointID != "" || action.Capture != nil {
		t.Fatal("creation fabricated a checkpoint ID")
	}
	if !action.Confirm || action.Kind != session.Effect {
		t.Fatal("creation bypasses explicit control")
	}
	guidance := action.Reconciliation(request)
	if len(guidance) != 5 || guidance[4] != "list" {
		t.Fatalf("unknown identity guidance=%q", guidance)
	}
	action.Run(t.Context(), request)
	if len(probe.requests) != 1 || probe.requests[0].Cwd != "/project" || probe.requests[0].Root != "/state" {
		t.Fatalf("target changed: %+v", probe.requests)
	}
}

func TestCreateFormCannotBypassConflictingTargetValidation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent-state")
	backend := app.NewWorkbench(app.Dependencies{Environment: []string{}})
	action := Actions(backend, app.Arguments{Root: root}, t.TempDir())[0]
	values := make([]string, len(action.Fields))
	for i, field := range action.Fields {
		values[i] = field.Value
		if field.Label == "Workspace ID" {
			values[i] = strings.Repeat("a", 32)
		}
	}
	request, err := action.Prepare(values)
	if err != nil {
		t.Fatal(err)
	}
	if !action.Confirm || request.Cwd == "" || request.WorkspaceID == "" {
		t.Fatal("test did not submit both explicit targets")
	}
	feedback := action.Run(t.Context(), request)
	if feedback.Err == nil || !strings.Contains(feedback.Err.Error(), "exactly one") {
		t.Fatalf("conflicting checkpoint targets reached mutation: %+v", feedback)
	}
	if _, err = os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid form initialized state: %v", err)
	}
}
