// Package checkpoint presents explicit Git checkpoint controls without owning
// checkpoint records, helper processes or cleanup decisions.
package checkpoint

import (
	"context"
	"fmt"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

type Backend interface {
	Checkpoint(context.Context, app.Arguments) app.OperationResult
}

func Actions(backend Backend, base app.Arguments, cwd string) []ui.Action {
	var result []ui.Action
	for _, operation := range []string{"create", "status", "list", "remove"} {
		a := ui.Defaults("checkpoint", operation)
		a.Root = base.Root
		result = append(result, ui.Action{ID: "checkpoint-" + operation, Label: "Checkpoint: " + operation, Description: "Creation/removal use the existing finite helper lifecycle. Explicit inspection may reconcile acknowledged helper work; uncertain operations are never retried automatically.", Arguments: a, Fields: fields(operation, cwd), Kind: session.Effect, Confirm: operation == "create" || operation == "remove", Reconciliation: readback, Run: func(ctx context.Context, a app.Arguments) ui.Feedback { return present(backend.Checkpoint(ctx, a)) }})
	}
	return result
}

func fields(operation, cwd string) []ui.Field {
	if operation == "list" {
		return []ui.Field{ui.TextField("After", "optional checkpoint cursor", "", func(a *app.Arguments, s string) { a.CheckpointAfter = s })}
	}
	if operation == "create" {
		return []ui.Field{
			ui.TextField("Working directory", "choose this ordinary Git checkout OR a managed workspace", cwd, func(a *app.Arguments, s string) { a.Cwd = s }),
			ui.TextField("Workspace ID", "leave directory empty to select a managed workspace", "", func(a *app.Arguments, s string) { a.WorkspaceID = s }),
			ui.TextField("Budget", "finite helper duration; blank keeps existing default", "", func(a *app.Arguments, s string) { a.CheckpointBudget = s }),
		}
	}
	result := []ui.Field{ui.TextField("Checkpoint ID", "exact checkpoint ID", "", func(a *app.Arguments, s string) { a.CheckpointID = s })}
	if operation == "status" {
		return append(result, ui.TextField("Operation ID", "optional acknowledged removal operation", "", func(a *app.Arguments, s string) { a.CheckpointOperationID = s }))
	}
	return append(result, ui.TextField("Budget", "finite removal helper duration; blank keeps default", "", func(a *app.Arguments, s string) { a.CheckpointBudget = s }))
}

func readback(a app.Arguments) []string {
	if a.CheckpointID == "" {
		return []string{"delegate", "--root", a.Root, "checkpoint", "list"}
	}
	result := []string{"delegate", "--root", a.Root, "checkpoint", "status", a.CheckpointID}
	if a.CheckpointOperationID != "" {
		result = append(result, "--operation", a.CheckpointOperationID)
	}
	return result
}

func present(result app.OperationResult) ui.Feedback {
	feedback := ui.Result(result)
	var text strings.Builder
	if items := result.Response.Checkpoints; items != nil {
		for _, item := range *items {
			fmt.Fprintf(&text, "%s  %s  %s\n", item.ID, item.State, item.Cwd)
		}
	}
	if item := result.Response.Checkpoint; item != nil {
		fmt.Fprintf(&text, "Checkpoint: %s\nOperation: %s\nReason: %s\nNext: %s\n", item.ID, item.OperationID, item.ReasonCode, item.NextAction)
		if current := item.Current; current != nil {
			fmt.Fprintf(&text, "State: %s\nUsable: %t\nDirectory: %s\nWorkspace: %s\n", current.State, current.Usable, current.Cwd, current.WorkspaceID)
		}
	}
	if cursor := result.Response.CheckpointCursor; cursor != "" {
		fmt.Fprintf(&text, "Next cursor: %s\n", cursor)
	}
	feedback.Title = "Checkpoint"
	feedback.Body = text.String() + feedback.Body
	return feedback
}
