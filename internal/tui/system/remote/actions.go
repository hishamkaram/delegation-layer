// Package remote presents explicit origin-reference reconciliation and selected
// byte-family retirement. Destination/origin owners keep their existing guards.
package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

type Backend interface {
	Remote(context.Context, app.Arguments) app.OperationResult
}

func Actions(backend Backend, base app.Arguments) []ui.Action {
	var result []ui.Action
	for _, operation := range []string{"list", "reconcile", "usage", "retire-preview", "retire-apply"} {
		command := operation
		if strings.HasPrefix(operation, "retire-") {
			command = "retire"
		}
		a := ui.Defaults("remote", command)
		a.Root = base.Root
		a.RemoteDryRun = operation == "retire-preview"
		a.RemoteApply = operation == "retire-apply"
		kind := session.Effect
		if command == "list" {
			kind = session.Read
		}
		result = append(result, ui.Action{
			ID: "remote-" + operation, Label: "Remote: " + operation, Description: "Only existing origin references and explicitly selected owned bytes are affected. Active-use and destination guards remain unchanged; no broad cleanup.", Arguments: a, Fields: fields(command), Kind: kind, Confirm: operation == "retire-apply", Reconciliation: func(a app.Arguments) []string {
				if a.TaskID == "" {
					return []string{"delegate", "--root", a.Root, "remote", "list"}
				}
				return []string{"delegate", "--root", a.Root, "remote", "usage", a.TaskID, "--scope", a.RemoteScope}
			}, Run: func(ctx context.Context, a app.Arguments) ui.Feedback {
				return present(backend.Remote(ctx, a), a.RemoteAction)
			},
		})
	}
	return result
}

func fields(command string) []ui.Field {
	if command == "list" {
		return []ui.Field{ui.TextField("After", "optional origin reference cursor", "", func(a *app.Arguments, s string) { a.RemoteAfter = s })}
	}
	result := []ui.Field{ui.TextField("Remote reference", "exact remote:ID", "", func(a *app.Arguments, s string) { a.TaskID = s })}
	if command == "usage" || command == "retire" {
		result = append(result, ui.TextField("Scope", "origin, destination or both", "both", func(a *app.Arguments, s string) { a.RemoteScope = s }))
	}
	return result
}

func present(operation app.OperationResult, action string) ui.Feedback {
	feedback := ui.Result(operation)
	feedback.Title = "Remote " + action
	if operation.Remote == nil {
		return feedback
	}
	report := operation.Remote
	var text strings.Builder
	fmt.Fprintf(&text, "Reference: %s\nConnectivity: %s\n", report.TaskID, report.Connectivity)
	for _, id := range report.References {
		fmt.Fprintln(&text, id)
	}
	if report.NextCursor != "" {
		fmt.Fprintf(&text, "Next cursor: %s\n", report.NextCursor)
	}
	if action == "usage" || action == "retire" {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			feedback.Err = err
		} else {
			text.Write(encoded)
		}
	}
	feedback.Body = text.String() + feedback.Body
	return feedback
}
