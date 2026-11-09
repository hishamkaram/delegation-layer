package tui

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/history"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
	"github.com/hishamkaram/delegation-layer/internal/tui/browse"
	"github.com/hishamkaram/delegation-layer/internal/tui/schedules"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/system"
	"github.com/hishamkaram/delegation-layer/internal/tui/tasks"
	"github.com/hishamkaram/delegation-layer/internal/tui/teams"
	"github.com/hishamkaram/delegation-layer/internal/tui/terminal"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

// Launch composes a terminal frontend, never a new task or scheduler owner.
func Launch(a app.Arguments, deps app.Dependencies, input *os.File, output, stderr io.Writer) int {
	root, err := tasklifecycle.ResolveRoot(a.Root)
	if err != nil {
		return reportRunError(stderr, err.Error(), 2)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return reportRunError(stderr, err.Error(), 1)
	}
	a.Root = root
	if deps.Environment == nil {
		deps.Environment = os.Environ()
	}
	if deps.Catalog.IsZero() {
		deps.Catalog = app.NativeCatalog()
	}
	registry := deps.Catalog.Registry()
	if deps.PredicateRegistry != nil {
		registry = deps.PredicateRegistry()
	}
	historySession := browse.New(history.Options{Root: root, Environment: deps.Environment, Registry: registry})
	sink := &diagnosticSink{events: make(chan diagnosticMsg, 50)}
	deps.ReportDiagnostic = sink.Report
	workbench := app.NewWorkbench(deps)
	taskSection := historySection(workbench, a, cwd, historySession)
	sections := []ui.Section{taskSection, teams.NewSection(workbench, a, cwd), schedules.NewSection(workbench, a, cwd), system.NewSection(workbench, a, cwd)}
	options := Options{Root: root, Cwd: cwd, Version: deps.PublisherVersion, Environment: deps.Environment, Input: input, Output: output, Error: stderr, Sections: sections}
	options.Start = func(tracker *session.Tracker, publish func(tea.Msg)) error {
		_, sinkErr := tracker.Start(session.Read, session.Reference{Description: "diagnostic display"}, func(ctx context.Context) error { return sink.Drain(ctx, publish) }, func(_ uint64, err error) { publish(finishedMsg{err: err}) })
		if sinkErr != nil {
			return sinkErr
		}
		_, err := tracker.Start(session.Read, session.Reference{Description: "history reconciliation"}, func(ctx context.Context) error { return historySession.Maintain(ctx, sink.Report) }, func(_ uint64, err error) { publish(finishedMsg{err: err}) })
		return err
	}
	options.CheckContext = historySession.AssertBinding
	options.Close = historySession.Close
	code := Run(options)
	sink.Close()
	return code
}

func historySection(backend *app.Workbench, a app.Arguments, cwd string, source *browse.Session) ui.Section {
	section := tasks.NewSection(backend, a, cwd, nil)
	section.Load = func(ctx context.Context, request ui.PageRequest) (ui.Page, error) {
		c, cacheErr := source.Catalog()
		page, err := tasks.NewSection(backend, a, cwd, c).Load(ctx, request)
		if cacheErr != nil {
			page.Summary = "History unavailable: " + cacheErr.Error() + "\n" + page.Summary
		}
		if description := source.Description(); description != "" {
			page.Summary = description + "\n" + page.Summary
		}
		return page, err
	}
	section.Actions = append(section.Actions,
		ui.Action{ID: "history-refresh", Label: "Refresh task discovery", Kind: session.Read, Arguments: a, Run: func(context.Context, app.Arguments) ui.Feedback {
			source.RequestRefresh()
			return ui.Feedback{Body: "History discovery refresh requested; source tasks are unchanged."}
		}},
		ui.Action{ID: "history-rebuild", Label: "Rebuild disposable history cache", Kind: session.Effect, Confirm: true, Arguments: a, Description: "Rebuild derived metadata only. Another open TUI may defer this; task records are never removed.", Run: func(ctx context.Context, _ app.Arguments) ui.Feedback {
			err := source.Rebuild(ctx)
			return ui.Feedback{Body: "History cache rebuild requested", Err: err}
		}},
	)
	return section
}

// diagnosticSink has one invocation owner. Late callbacks never write to the
// terminal, and a full diagnostics queue never blocks infrastructure cleanup.
type diagnosticSink struct {
	mu     sync.Mutex
	events chan diagnosticMsg
	closed bool
}

func (d *diagnosticSink) Close() { d.mu.Lock(); defer d.mu.Unlock(); d.closed = true }
func (d *diagnosticSink) Drain(ctx context.Context, publish func(tea.Msg)) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case text := <-d.events:
			publish(text)
		}
	}
}

func (d *diagnosticSink) Report(err error) {
	if err == nil {
		return
	}
	d.mu.Lock()
	closed := d.closed
	d.mu.Unlock()
	if closed {
		return
	}
	text := terminal.Safe(err.Error())
	if len(text) > 2048 {
		text = text[:2048] + "…"
	}
	select {
	case d.events <- diagnosticMsg{text: text, rootInvalid: errors.Is(err, history.ErrRootChanged)}:
	default:
	}
}

var _ tasks.Backend = (*app.Workbench)(nil)
