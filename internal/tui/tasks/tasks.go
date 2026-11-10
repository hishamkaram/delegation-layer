// Package tasks presents task history and explicit lifecycle actions. It owns
// view/form values only; task lifecycle and history remain their own owners.
package tasks

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/history"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

type Backend interface {
	PreviewDispatch(context.Context, app.Arguments) (app.DispatchPreview, error)
	Dispatch(context.Context, app.Arguments) app.OperationResult
	Continue(context.Context, app.Arguments) app.OperationResult
	Inspect(context.Context, app.Arguments) app.OperationResult
	Collect(context.Context, app.Arguments) app.OperationResult
	Cancel(context.Context, app.Arguments) app.OperationResult
	Recheck(app.Arguments) app.OperationResult
	Events(context.Context, app.Arguments) app.OperationResult
	Reply(context.Context, app.Arguments) app.OperationResult
	Steer(context.Context, app.Arguments) app.OperationResult
	Logs(context.Context, app.Arguments) app.OperationResult
	Wait(context.Context, app.Arguments) app.OperationResult
	ReadPublishedResult(context.Context, app.Arguments, int64) (app.ContentPreview, error)
	ReadLogRange(context.Context, app.Arguments) (taskdir.LogRange, error)
}

type views struct {
	backend Backend
	base    app.Arguments
	cwd     string
	catalog *history.Catalog
}

func NewSection(backend Backend, base app.Arguments, cwd string, catalog *history.Catalog) ui.Section {
	v := views{backend: backend, base: base, cwd: cwd, catalog: catalog}
	return ui.Section{Name: "Tasks", Load: v.load, Actions: v.globalActions()}
}

func (v views) defaults(path ...string) app.Arguments {
	a := ui.Defaults(path...)
	a.Root, a.PueueConfig, a.Runner = v.base.Root, v.base.PueueConfig, v.base.Runner
	a.SSHConfig = v.base.SSHConfig
	return a
}

func (v views) load(ctx context.Context, request ui.PageRequest) (ui.Page, error) {
	page := ui.Page{}
	if v.catalog == nil {
		page.Summary = "Global history is unavailable. Exact-ID actions remain available; no page-only search is substituted."
		return page, nil
	}
	query, err := searchQuery(request.Search)
	if err != nil {
		return page, err
	}
	query.After = request.After
	result, err := v.catalog.Query(ctx, query)
	if err != nil {
		return page, err
	}
	page.Next = result.Next
	page.Columns = taskColumns()
	page.Strip, page.Coverage = coverage(result.Progress, query.Order)
	identities := make([]history.Identity, len(result.Rows))
	for i, row := range result.Rows {
		identities[i] = row.Identity
	}
	observations, err := v.catalog.ObserveBatch(ctx, identities)
	if err != nil {
		return page, err
	}
	for i, row := range result.Rows {
		// A row fault retains its prior evidence; source rebinding or cancelled
		// reads fail the entire page through the catalog's top-level error.
		if observations[i].Err == nil {
			row.Observation = &observations[i].Observation
		} else {
			row.ObservationFault = "visible passive refresh unavailable; previous evidence retained"
		}
		page.Entries = append(page.Entries, v.entry(row))
	}
	return page, nil
}

func taskColumns() []ui.Column {
	return []ui.Column{{Title: "Task ID", Width: 16}, {Title: "Provider", Width: 18}, {Title: "Admission", Width: 14}, {Title: "Liveness", Width: 14}, {Title: "Publication", Width: 14}, {Title: "Created", Width: 20}}
}

func coverage(value history.Progress, order history.Order) (strip, detail string) {
	state := "Discovery incomplete; ordering is recent among indexed records."
	compact := "discovery incomplete"
	if !value.Incomplete {
		state = "Last full metadata scan: " + value.LastScan.Format(time.RFC3339)
		compact = "scanned " + value.LastScan.Format(time.RFC3339)
	}
	ordering := "Order: created (unknown creation times remain unknown)."
	if order == history.OrderObserved {
		ordering = "Order: last observed (dated evidence, not creation time)."
	}
	strip = fmt.Sprintf("Dated • %s • %d indexed • %d observed • %d unobserved • %d faults • order:%s", compact, value.Indexed, value.Observed, value.Unobserved, value.Faults, order)
	detail = ordering + "\n" + fmt.Sprintf("%d indexed • %d last-observed • %d unobserved • %d faults\n%s\nMetadata search; filters: publication:committed admission:admitted liveness:ended status:timed_out; order:created or order:observed. Filters use dated catalog observations, not current global status.",
		value.Indexed, value.Observed, value.Unobserved, value.Faults, state)
	return strip, detail
}

func searchQuery(text string) (history.Query, error) {
	query := history.Query{Order: history.OrderCreated}
	var terms []string
	seen := map[string]bool{}
	for _, word := range strings.Fields(text) {
		key, value, found := strings.Cut(word, ":")
		if !found {
			terms = append(terms, word)
			continue
		}
		var target *string
		switch key {
		case "admission":
			target = &query.Admission
		case "liveness":
			target = &query.Liveness
		case "publication":
			target = &query.Publication
		case "status":
			target = &query.Status
		case "order":
			if seen[key] || (value != string(history.OrderCreated) && value != string(history.OrderObserved)) {
				return query, fmt.Errorf("order requires one value: created or observed")
			}
			seen[key] = true
			query.Order = history.Order(value)
			continue
		default:
			terms = append(terms, word)
			continue
		}
		if value == "" || seen[key] {
			return query, fmt.Errorf("filter %s requires one nonempty value", key)
		}
		seen[key], *target = true, value
	}
	query.Search = strings.Join(terms, " ")
	if len(seen) == 0 {
		query.Search = text
	}
	return query, nil
}

func (v views) entry(row history.Row) ui.Entry {
	m := row.Metadata
	created := "unknown"
	if m.CreatedAtKnown {
		created = m.CreatedAt.UTC().Format(time.RFC3339)
	}
	cells := []ui.Cell{{Text: row.Handle()}, {Text: m.Provider}, {Text: "not observed"}, {Text: "not observed"}, {Text: "not observed"}, {Text: created}}
	cells[0].Fault = row.Fault != ""
	for i := 2; i <= 4; i++ {
		cells[i].Fault = row.ObservationFault != ""
	}
	groups := []ui.DetailGroup{
		{Title: "Identity", Fields: []ui.DetailField{{Label: "Task", Value: row.Handle()}, {Label: "Created", Value: created}, {Label: "Remote device", Value: row.Device}}},
		{Title: "Request/context", Fields: []ui.DetailField{{Label: "Provider", Value: m.Provider}, {Label: "Requested model", Value: m.Model}, {Label: "Requested effort", Value: m.Effort}, {Label: "Working directory", Value: m.Cwd}, {Label: "Workspace", Value: m.WorkspaceID}, {Label: "Team/member", Value: m.TeamID + "/" + m.MemberID}}},
	}
	evidence := ui.DetailGroup{Title: "Evidence", Fields: []ui.DetailField{{Label: "Observation", Value: "not observed"}}}
	if observed := row.Observation; observed != nil {
		cells[2].Text, cells[3].Text, cells[4].Text = observed.Admission.String(), observed.Liveness.String(), observed.Publication.String()
		cells[2].Role, cells[3].Role, cells[4].Role = admissionRole(observed.Admission), livenessRole(observed.Liveness), publicationRole(observed.Publication)
		// Current nonempty statuses are stop causes; publication carries committed/rejected outcomes.
		failureRole := ui.RoleNeutral
		if observed.Status != "" || observed.FailureCode != "" {
			failureRole = ui.RoleNegative
		}
		evidence.Fields = []ui.DetailField{{Label: "Admission", Value: cells[2].Text, Role: cells[2].Role}, {Label: "Liveness", Value: cells[3].Text, Role: cells[3].Role}, {Label: "Publication", Value: cells[4].Text, Role: cells[4].Role}, {Label: "Status", Value: observed.Status, Role: failureRole}, {Label: "Failure code", Value: observed.FailureCode, Role: failureRole}, {Label: "Observed", Value: observed.At.Format(time.RFC3339)}, {Label: "Source", Value: observed.Source}}
	}
	groups = append(groups, evidence)
	limitations := ui.DetailGroup{Title: "Faults/limitations"}
	for _, fault := range []string{row.Fault, row.ObservationFault} {
		if fault != "" {
			limitations.Fields = append(limitations.Fields, ui.DetailField{Label: "Fault", Value: fault, Role: ui.RoleNegative})
		}
	}
	limitations.Fields = append(limitations.Fields, ui.DetailField{Label: "Authority", Value: "Cached rows grant no action authority. Inspect explicitly for live status; collect explicitly to recover publication."})
	groups = append(groups, limitations)
	return ui.Entry{ID: row.Handle(), Title: row.Handle(), Cells: cells, Details: groups, Actions: v.taskActions(row.Handle())}
}

func admissionRole(value task.Admission) ui.DisplayRole {
	switch value {
	case task.AdmissionAdmitted:
		return ui.RolePositive
	case task.AdmissionNotAdmitted:
		return ui.RoleNegative
	case task.AdmissionUnknown:
		return ui.RoleNeutral
	}
	return ui.RoleNeutral
}

func livenessRole(value task.Liveness) ui.DisplayRole {
	switch value {
	case task.LivenessRunning:
		return ui.RoleWarning
	case task.LivenessEnded, task.LivenessUndetermined:
		return ui.RoleNeutral
	}
	return ui.RoleNeutral
}

func publicationRole(value task.Publication) ui.DisplayRole {
	switch value {
	case task.PublicationCommitted:
		return ui.RolePositive
	case task.PublicationRejected:
		return ui.RoleNegative
	case task.PublicationPending:
		return ui.RoleWarning
	case task.PublicationUnknown:
		return ui.RoleNeutral
	}
	return ui.RoleNeutral
}
