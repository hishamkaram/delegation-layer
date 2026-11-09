// Package workspace presents explicit managed-workspace operations. The
// workspaces application owner remains the only state and cleanup authority.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

type Backend interface {
	Workspace(context.Context, app.Arguments) app.OperationResult
}

func Actions(backend Backend, base app.Arguments) []ui.Action {
	var result []ui.Action
	for _, operation := range []string{"list", "status", "exec", "release", "forget"} {
		a := ui.Defaults("workspace", operation)
		a.Root = base.Root
		result = append(result, ui.Action{ID: "workspace-" + operation, Label: "Workspace: " + operation, Description: description(operation), Arguments: a, Kind: session.Effect, Confirm: operation == "exec" || operation == "release" || operation == "forget", Fields: fields(operation), Reconciliation: readback, Run: func(ctx context.Context, a app.Arguments) ui.Feedback { return present(backend.Workspace(ctx, a)) }})
	}
	return result
}

func description(operation string) string {
	if operation == "exec" {
		return "Run one explicit argv list within the existing finite workspace operation. No shell expansion. Output remains captured."
	}
	if operation == "release" || operation == "forget" {
		return "Existing active-use/capture guards remain authoritative. Only this selected managed workspace is affected."
	}
	return "Explicit resource inspection can reconcile existing helper work; it never cancels provider tasks."
}

func fields(operation string) []ui.Field {
	if operation == "list" {
		return []ui.Field{ui.TextField("After", "optional workspace cursor", "", func(a *app.Arguments, s string) { a.WorkspaceAfter = s })}
	}
	result := []ui.Field{ui.TextField("Workspace ID", "exact managed workspace ID", "", func(a *app.Arguments, s string) { a.WorkspaceID = s })}
	if operation == "status" {
		return append(result, ui.TextField("Operation ID", "optional acknowledged operation", "", func(a *app.Arguments, s string) { a.WorkspaceOperationID = s }))
	}
	result = append(result, ui.TextField("Budget", "finite operation duration; blank keeps existing default", "", func(a *app.Arguments, s string) { a.WorkspaceBudget = s }))
	if operation == "release" {
		result = append(result, ui.Field{Label: "Discard uncaptured content", Hint: "true explicitly accepts loss of uncaptured checkout content", Value: "false", Apply: func(a *app.Arguments, s string) error {
			value, err := strconv.ParseBool(s)
			if err == nil {
				a.Discard = value
			}
			return err
		}})
	}
	if operation == "exec" {
		result = append(result, ui.Field{Label: "Command argv", Hint: "one argument per line; first line is the executable; one final newline ends the last argument; extra blank lines encode empty args; spaces/quotes are literal", Multiline: true, Apply: applyArgv})
	}
	return result
}

func applyArgv(a *app.Arguments, text string) error {
	args := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(args) == 0 || args[0] == "" {
		return errors.New("a command executable is required")
	}
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) {
			return errors.New("command argv contains NUL")
		}
	}
	a.ExecArguments = args
	return nil
}

func readback(a app.Arguments) []string {
	result := []string{"delegate", "--root", a.Root, "workspace", "status", a.WorkspaceID}
	if a.WorkspaceID == "" {
		result = []string{"delegate", "--root", a.Root, "workspace", "list"}
	}
	return result
}

func present(result app.OperationResult) ui.Feedback {
	feedback := ui.Result(result)
	var text strings.Builder
	for _, item := range result.Response.Workspaces {
		fmt.Fprintf(&text, "%s  %s  %s\n", item.ID, item.State, item.Path)
	}
	if item := result.Response.Workspace; item != nil {
		fmt.Fprintf(&text, "Workspace: %s\nState: %s\nPath: %s\nOperation: %s\nEvidence: %s\nReason: %s\nNext: %s\n", item.ID, item.State, item.Path, item.OperationID, item.EvidenceState, item.ReasonCode, item.NextAction)
		for _, artifact := range item.Artifacts {
			fmt.Fprintf(&text, "Artifact: %s (%s)\n", artifact.Path, artifact.EvidenceState)
		}
	}
	if cursor := result.Response.WorkspaceCursor; cursor != "" {
		fmt.Fprintf(&text, "Next cursor: %s\n", cursor)
	}
	feedback.Title = "Workspace"
	feedback.Body = text.String() + feedback.Body
	return feedback
}
