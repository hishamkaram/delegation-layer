package tasks

import (
	"context"
	"fmt"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func (v views) dispatch(remote bool) ui.Action {
	a := v.defaults("dispatch")
	fields := []ui.Field{textField("Provider", "exact installed provider; local auto/Team may leave blank", a.Provider, func(a *app.Arguments, s string) { a.Provider = s })}
	label := "Dispatch local task"
	var optional []ui.Field
	if remote {
		label = "Dispatch remote task"
		fields = append(fields, v.remoteTargetFields()...)
		optional = v.remoteAdvancedFields()
	} else {
		fields = append(fields, textField("Working directory", "absolute; leave empty when attaching an existing managed workspace", v.cwd, func(a *app.Arguments, s string) { a.Cwd = s }))
		optional = append(optional,
			booleanField("Auto provider", "true or false; mutually exclusive with explicit provider", a.Auto, func(a *app.Arguments, b bool) { a.Auto = b }),
			textField("Isolation", "optional git-worktree", a.Isolation, func(a *app.Arguments, s string) { a.Isolation = s }),
			textField("Workspace ID", "attach to an existing managed workspace", a.WorkspaceID, func(a *app.Arguments, s string) { a.WorkspaceID = s }),
			textField("Base revision", "optional managed worktree base", a.Base, func(a *app.Arguments, s string) { a.Base = s }),
			textField("Workspace helper budget", "optional finite helper budget", a.WorkspaceBudget, func(a *app.Arguments, s string) { a.WorkspaceBudget = s }))
		optional = append(optional, teamFields()...)
	}
	fields = append(fields, briefFields()...)
	fields = append(fields, executionFields(a)...)
	fields = append(fields, advanced(optional...)...)
	return ui.Action{
		ID: label, Label: label, Description: "Submit exactly one request. Review destination, provider, requested options and permission intent. An uncertain admission is inspected, never automatically retried.",
		Arguments: a, Fields: fields, Kind: session.Effect, Confirm: true, Capture: captureTaskID,
		Preview: v.previewDispatch,
		Reconciliation: func(a app.Arguments) []string {
			handle := a.TaskID
			if remote {
				handle = "remote:" + handle
			}
			return readback(a, "status", handle)
		}, Run: func(ctx context.Context, a app.Arguments) ui.Feedback {
			return taskFeedback(v.backend.Dispatch(ctx, a))
		},
	}
}

func captureTaskID(a *app.Arguments) error {
	id, err := randomid.NewTaskID()
	if err == nil {
		a.TaskID = id
	}
	return err
}

func (v views) previewDispatch(ctx context.Context, a app.Arguments) (ui.Preview, error) {
	resolved, err := v.backend.PreviewDispatch(ctx, a)
	if err != nil {
		return ui.Preview{}, err
	}
	provider := resolved.Provider
	if resolved.Auto {
		provider = "automatic selection by the existing admission owner"
	}
	summary := fmt.Sprintf("Resolved request\nProvider: %s\nModel: %s\nEffort: %s\nPermission: %s\nBudget: %s\nOutput silence timeout: %s\nBlank model/effort values retain the provider default; no value is invented.",
		provider, resolved.Config.Model, resolved.Config.Effort, resolved.Config.Permission, resolved.Config.Budget, resolved.Config.OutputSilenceTimeout)
	if resolved.Team != nil {
		summary += fmt.Sprintf("\nTeam/member: %s/%s\nTeam revision: %d\nThis selected catalog/revision is pinned; drift requires a new review.",
			resolved.Team.Target.Team.TeamID, resolved.Team.Target.MemberID, resolved.Team.TeamRevision)
	}
	return ui.Preview{Arguments: resolved.Arguments, Summary: summary}, nil
}

func (v views) remoteTargetFields() []ui.Field {
	return []ui.Field{
		textField("SSH device", "existing SSH configuration alias; no automatic trust", "", func(a *app.Arguments, s string) { a.Device = s }),
		textField("Destination root", "absolute destination state root", "", func(a *app.Arguments, s string) { a.RemoteRoot = s }),
		textField("Destination working directory", "absolute; or choose an input manifest", "", func(a *app.Arguments, s string) { a.RemoteCwd = s }),
	}
}

func (v views) remoteAdvancedFields() []ui.Field {
	return []ui.Field{
		textField("SSH configuration", "optional existing user-owned config", v.base.SSHConfig, func(a *app.Arguments, s string) { a.SSHConfig = s }),
		textField("Destination executable", "installed Delegate executable; launched in place", "delegate", func(a *app.Arguments, s string) { a.RemoteExecutable = s }),
		textField("Input manifest", "optional existing input selection manifest", "", func(a *app.Arguments, s string) { a.InputManifest = s }),
		textField("Output manifest", "optional existing output selection manifest", "", func(a *app.Arguments, s string) { a.OutputManifest = s }),
	}
}

func (v views) continueTask(id string) ui.Action {
	a := v.defaults("continue")
	a.TaskID = id
	fields := []ui.Field{textField("Predecessor task", "exact local predecessor; permission/session/workspace are inherited", id, func(a *app.Arguments, s string) { a.TaskID = s })}
	fields = append(fields, briefFields()...)
	fields = append(fields, textField("Budget", "finite successor budget", a.Config.Budget, func(a *app.Arguments, s string) { a.Config.Budget = s }))
	fields = append(fields, advanced(
		textField("Requested model", "optional supported continuation override", a.Config.Model, func(a *app.Arguments, s string) { a.Config.Model = s }),
		textField("Effort", "optional supported continuation override", a.Config.Effort, func(a *app.Arguments, s string) { a.Config.Effort = s }))...)
	fields = append(fields, advanced(resourceGuardFields(a)...)...)

	return ui.Action{
		ID: "task-continue", Label: "Continue local task", Description: "Continue only through the recorded exact session and current lifecycle eligibility. Permission is inherited; no fallback or retry.", Arguments: a, Fields: fields, Kind: session.Effect, Confirm: true,
		Capture: func(a *app.Arguments) error {
			id, err := randomid.NewTaskID()
			if err == nil {
				a.SuccessorTaskID = id
			}
			return err
		}, Reconciliation: func(a app.Arguments) []string { return readback(a, "status", a.SuccessorTaskID) },
		Run: func(ctx context.Context, a app.Arguments) ui.Feedback {
			return taskFeedback(v.backend.Continue(ctx, a))
		},
	}
}
