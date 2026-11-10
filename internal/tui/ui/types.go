// Package ui contains terminal presentation values shared by the workbench
// views. Application owners remain responsible for validation and mutation.
package ui

import (
	"context"
	"fmt"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
)

// Field binds one editor to a typed request field. Apply is pure input parsing;
// authoritative application validation still runs when the user submits.
type Field struct {
	Advanced           bool
	Label, Hint, Value string
	Multiline          bool
	Apply              func(*app.Arguments, string) error
}

type Feedback struct {
	ResetPage       bool
	ContextInvalid  bool
	Title, Body     string
	Err             error
	RestartRequired bool
}

type Preview struct {
	Arguments app.Arguments
	Summary   string
}

type Action struct {
	KindFor                func(app.Arguments) session.Kind
	Preview                func(context.Context, app.Arguments) (Preview, error)
	ID, Label, Description string
	Fields                 []Field
	Arguments              app.Arguments
	Kind                   session.Kind
	Confirm                bool
	Disabled               string
	Reconcile              []string
	Capture                func(*app.Arguments) error
	Reconciliation         func(app.Arguments) []string
	Run                    func(context.Context, app.Arguments) Feedback
}

func (a Action) Prepare(values []string) (app.Arguments, error) {
	if len(values) != len(a.Fields) {
		return app.Arguments{}, fmt.Errorf("form field count changed; reopen this action")
	}
	request := a.Arguments
	for i, field := range a.Fields {
		if field.Apply != nil {
			if err := field.Apply(&request, values[i]); err != nil {
				return app.Arguments{}, fmt.Errorf("%s: %w", field.Label, err)
			}
		}
	}
	if a.Capture != nil {
		if err := a.Capture(&request); err != nil {
			return app.Arguments{}, err
		}
	}
	return request, nil
}

type Entry struct {
	ID, Title, Subtitle, Detail string
	Actions                     []Action
	Cells                       []Cell
	Details                     []DetailGroup
}

type Page struct {
	Entries       []Entry
	Actions       []Action
	Summary, Next string
	Columns       []Column
	Strip         string
	Coverage      string
}

// Column and Cell describe display values, never task identity or authority.
// Table entries have one cell per column; the first column keeps identity visible.
type Column struct {
	Title string
	Width int
}

type Cell struct {
	Text  string
	Fault bool
}

// DetailGroup is the single structured source for task detail presentations.
type DetailGroup struct {
	Title  string
	Fields []DetailField
}

type DetailField struct{ Label, Value string }

type PageRequest struct{ Search, After string }

type Section struct {
	Name    string
	Load    func(context.Context, PageRequest) (Page, error)
	Actions []Action
}

// Defaults derives defaults from the same actual registration used by CLI/help.
func Defaults(path ...string) app.Arguments {
	a, err := app.CommandDefaults(path)
	if err != nil {
		panic(fmt.Sprintf("invalid built-in UI action %v: %v", path, err))
	}
	return a
}

func Result(result app.OperationResult) Feedback {
	response := result.Response
	var lines []string
	if response.TaskID != "" {
		lines = append(lines, "Task: "+response.TaskID)
	}
	if response.Admission != "" {
		lines = append(lines, "Admission: "+response.Admission, "Liveness: "+response.Liveness, "Publication: "+response.Publication)
	}
	if response.Status != "" {
		lines = append(lines, "Status: "+response.Status)
	}
	if response.Failure != nil {
		lines = append(lines, "Failure: "+response.Failure.Code, "Next action: "+response.Failure.NextAction)
	}
	if response.Pending != nil {
		lines = append(lines, fmt.Sprintf("Pending: %+v", *response.Pending))
	}
	if result.Remote != nil {
		lines = append(lines, "Remote: "+result.Remote.TaskID, "Connectivity: "+result.Remote.Connectivity)
	}
	if result.Err != nil {
		lines = append(lines, result.Err.Error())
	}
	return Feedback{Title: response.Command, Body: strings.Join(lines, "\n"), Err: result.Err, RestartRequired: result.RestartRequired}
}
