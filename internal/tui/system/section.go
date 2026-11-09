// Package system presents explicit discovery and infrastructure controls. It
// owns no installation, provider configuration, task or resource authority.
package system

import (
	"context"
	"fmt"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/system/checkpoint"
	"github.com/hishamkaram/delegation-layer/internal/tui/system/remote"
	"github.com/hishamkaram/delegation-layer/internal/tui/system/workspace"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

type Backend interface {
	Providers() app.ProviderResponse
	Capabilities(string) (app.CapabilityResponse, error)
	Models(app.Arguments) (app.ModelsResponse, app.OperationResult)
	Doctor(app.Arguments) (app.DoctorReport, int)
	Preflight(context.Context, app.Arguments) app.OperationResult
	Supervisor(app.Arguments) app.OperationResult
	Scheduler(context.Context, app.Arguments) app.OperationResult
	Device(context.Context, app.Arguments) app.OperationResult
	workspace.Backend
	checkpoint.Backend
	remote.Backend
}

type view struct {
	backend Backend
	base    app.Arguments
	cwd     string
}

func NewSection(backend Backend, base app.Arguments, cwd string) ui.Section {
	v := view{backend: backend, base: base, cwd: cwd}
	actions := append(v.infrastructureActions(), v.discoveryActions()...)
	actions = append(actions, v.deviceActions()...)
	actions = append(actions, workspace.Actions(backend, base)...)
	actions = append(actions, checkpoint.Actions(backend, base, cwd)...)
	actions = append(actions, remote.Actions(backend, base)...)
	return ui.Section{Name: "System", Load: v.load, Actions: actions}
}

func (v view) defaults(path ...string) app.Arguments {
	a := ui.Defaults(path...)
	a.Root = v.base.Root
	return a
}

func (v view) load(_ context.Context, page ui.PageRequest) (ui.Page, error) {
	response := v.backend.Providers()
	result := ui.Page{Summary: "Delegate settings: execution root and explicit project/Team/schedule policy. Provider accounts/settings remain provider-owned.\nScheduler host: use Scheduler status for its root-specific foreground serve arguments. Start it independently; navigation never launches it."}
	result.Entries = append(result.Entries, ui.Entry{ID: "context", Title: "Current context", Subtitle: v.base.Root, Detail: fmt.Sprintf("Execution root: %s\nProject directory: %s\n\nNo environment or credentials are displayed.", v.base.Root, v.cwd)})
	for _, provider := range response.Providers {
		if page.Search != "" && !strings.Contains(strings.ToLower(provider.ID), strings.ToLower(page.Search)) {
			continue
		}
		result.Entries = append(result.Entries, ui.Entry{ID: provider.ID, Title: provider.ID, Subtitle: strings.Join(provider.SupportedModes, ", "), Detail: fmt.Sprintf("Provider: %s\nDeclared options: %s\nContinuation: %s\n\nCompiled capabilities do not prove authentication or runtime readiness.", provider.ID, strings.Join(provider.SupportedOptions, ", "), provider.Continuation), Actions: v.providerActions(provider.ID)})
	}
	return result, nil
}

func readback(a app.Arguments, args ...string) []string {
	return append([]string{"delegate", "--root", a.Root}, args...)
}

func (v view) infrastructureActions() []ui.Action {
	var actions []ui.Action
	for _, operation := range []string{"status", "migrate", "ensure-owner", "stop-owner"} {
		a := v.defaults("supervisor", operation)
		description := "Explicit supervisor operation; existing active-work and custody guards remain authoritative."
		if operation == "ensure-owner" {
			description = "Requires a prepared persistent user context outside any task-owned lifetime."
		}
		actions = append(actions, ui.Action{ID: "supervisor-" + operation, Label: "Supervisor: " + operation, Description: description, Arguments: a, Kind: session.Effect, Confirm: operation != "status", Reconciliation: func(a app.Arguments) []string { return readback(a, "supervisor", "status") }, Run: func(_ context.Context, a app.Arguments) ui.Feedback {
			return infrastructureFeedback(v.backend.Supervisor(a))
		}})
	}
	for _, operation := range []string{"status", "stop", "migrate"} {
		a := v.defaults("scheduler", operation)
		kind := session.Effect
		if operation == "status" {
			kind = session.Read
		}
		actions = append(actions, ui.Action{ID: "scheduler-" + operation, Label: "Scheduler: " + operation, Description: "Stop does not cancel admitted tasks. Migration requires the host stopped; no automatic recovery or launch.", Arguments: a, Kind: kind, Confirm: operation != "status", Reconciliation: func(a app.Arguments) []string { return readback(a, "scheduler", "status") }, Run: func(ctx context.Context, a app.Arguments) ui.Feedback {
			return infrastructureFeedback(v.backend.Scheduler(ctx, a))
		}})
	}
	return actions
}

func infrastructureFeedback(result app.OperationResult) ui.Feedback {
	feedback := ui.Result(result)
	var extra []string
	if s := result.Response.SupervisorControl; s != nil {
		extra = append(extra, fmt.Sprintf("Supervisor action: %s\nState: %s", s.Action, s.State))
	}
	if s := result.Response.SupervisorOwner; s != nil {
		extra = append(extra, fmt.Sprintf("Infrastructure owner: %+v", *s))
	}
	if s := result.Response.Scheduler; s != nil {
		extra = append(extra, fmt.Sprintf("Scheduler: %s\nReady: %t\nNext: %s\nForeground serve arguments: %q", s.State, s.Ready, s.NextAction, s.ServeCommand))
	}
	feedback.Body = strings.Join(append([]string{feedback.Body}, extra...), "\n")
	return feedback
}
