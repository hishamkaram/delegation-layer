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
	page.Next, page.Summary = result.Next, coverage(result.Progress, query.Order)
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

func coverage(value history.Progress, order history.Order) string {
	state := "Discovery incomplete; ordering is recent among indexed records."
	if !value.Incomplete {
		state = "Last full metadata scan: " + value.LastScan.Format(time.RFC3339)
	}
	ordering := "Order: created (unknown creation times remain unknown)."
	if order == history.OrderObserved {
		ordering = "Order: last observed (dated evidence, not creation time)."
	}
	return ordering + "\n" + fmt.Sprintf("%d indexed • %d last-observed • %d unobserved • %d faults\n%s\nMetadata search; filters: publication:committed admission:admitted liveness:ended status:timed_out; order:created or order:observed. Filters use dated catalog observations, not current global status.",
		value.Indexed, value.Observed, value.Unobserved, value.Faults, state)
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
		created = m.CreatedAt.Format(time.RFC3339)
	}
	state := "not observed"
	if row.Observation != nil {
		state = row.Observation.Publication.String() + " • " + row.Observation.Liveness.String()
	}
	detail := fmt.Sprintf("Task: %s\nProvider: %s\nRequested model: %s\nRequested effort: %s\nWorking directory: %s\nWorkspace: %s\nTeam/member: %s/%s\nRemote device: %s\nCreated: %s\n",
		row.Handle(), m.Provider, m.Model, m.Effort, m.Cwd, m.WorkspaceID, m.TeamID, m.MemberID, row.Device, created)
	if observed := row.Observation; observed != nil {
		detail += fmt.Sprintf("\nAdmission: %s\nLiveness: %s\nPublication: %s\nStatus: %s\nFailure code: %s\nObserved: %s (%s)\n",
			observed.Admission.String(), observed.Liveness.String(), observed.Publication.String(), observed.Status, observed.FailureCode, observed.At.Format(time.RFC3339), observed.Source)
	}
	for _, fault := range []string{row.Fault, row.ObservationFault} {
		if fault != "" {
			detail += "\nFault: " + fault
		}
	}
	detail += "\nCached rows grant no action authority. Inspect explicitly for live status; collect explicitly to recover publication."
	return ui.Entry{ID: row.Handle(), Title: row.Handle(), Subtitle: m.Provider + " • " + state, Detail: detail, Actions: v.taskActions(row.Handle())}
}
