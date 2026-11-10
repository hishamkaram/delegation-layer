package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/testutil/goroutinecheck"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func TestMain(m *testing.M)                    { os.Exit(goroutinecheck.Run(m)) }
func TestGoroutineCheckInstalled(t *testing.T) { goroutinecheck.VerifyInstalled(t) }
func fixtureModel(t *testing.T) *model {
	t.Helper()
	tracker := session.New(t.Context())
	t.Cleanup(func() {
		tracker.Stop()
		deadline := time.After(2 * time.Second)
		for len(tracker.Pending()) > 0 {
			select {
			case <-tracker.Changed():
			case <-deadline:
				t.Error("session workers did not close")
				return
			}
		}
	})
	m := newModel([]ui.Section{{Name: "Tasks"}, {Name: "Teams"}, {Name: "Schedules"}, {Name: "System"}}, "/private/state", "/project", "test", tracker, func(tea.Msg) {})
	m.resize(120, 30)
	return m
}

func TestStaleRefreshCannotReplaceCurrentView(t *testing.T) {
	m := fixtureModel(t)
	m.generation = 2
	m.selected = 1
	m.loaded(loadedMsg{generation: 1, section: 0, page: ui.Page{Summary: "stale"}})
	if m.page.Summary != "" {
		t.Fatal("stale response applied")
	}
	m.loaded(loadedMsg{generation: 2, section: 1, page: ui.Page{Summary: "current", Entries: []ui.Entry{{ID: "team", Title: "Team"}}}})
	if m.page.Summary != "current" {
		t.Fatal("current response lost")
	}
}

func TestUntrustedDetailsNeverReachTerminalControls(t *testing.T) {
	m := fixtureModel(t)
	m.page.Entries = []ui.Entry{{ID: "id", Title: "evil\x1b]52;c;clipboard\x07", Detail: "result\x1b[2J\x1b]0;title\x07"}}
	m.openDetail()
	view := m.View().Content
	if strings.Contains(view, "\x1b]52") || strings.Contains(view, "\x1b[2J") || strings.Contains(view, "\x1b]0;") {
		t.Fatalf("unsafe terminal content=%q", view)
	}
	if !strings.Contains(view, "result") {
		t.Fatal("safe result missing")
	}
}

func TestDuplicateSubmitAndQuitLeaveMutationContextAlive(t *testing.T) {
	m := fixtureModel(t)
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() { close(release); <-done })
	action := ui.Action{ID: "dispatch", Label: "Dispatch", Kind: session.Effect, Run: func(ctx context.Context, _ app.Arguments) ui.Feedback {
		started <- ctx
		<-release
		close(done)
		return ui.Feedback{}
	}}
	m.runPrepared(action, app.Arguments{})
	ctx := <-started
	m.runPrepared(action, app.Arguments{})
	if m.problem != "This operation is already in flight" {
		t.Fatal("duplicate not blocked")
	}
	m.beginQuit()
	if ctx.Err() != nil {
		t.Fatal("quit cancelled admission")
	}
	if len(m.tracker.Pending()) != 1 {
		t.Fatalf("pending=%+v", m.tracker.Pending())
	}
	if cmd := m.quitKey("f"); cmd != nil {
		t.Fatal("force did not require confirmation")
	}
	if cmd := m.quitKey("enter"); cmd == nil || m.exitCode != 3 {
		t.Fatal("force exit contract missing")
	}
}

func TestSmallTerminalCanRecoverAfterResize(t *testing.T) {
	m := fixtureModel(t)
	m.resize(20, 8)
	if !strings.Contains(m.View().Content, "too small") {
		t.Fatal("resize guidance absent")
	}
	m.resize(100, 25)
	if !strings.Contains(m.View().Content, "Tasks") {
		t.Fatal("view did not recover")
	}
}

func TestFirstSignalKeepsGraceWhileAlreadyClosing(t *testing.T) {
	m := fixtureModel(t)
	m.beginQuit()
	if cmd := m.workerMessage(signalMsg{code: 143}); cmd != nil {
		t.Fatal("first signal forced an already-closing session without its grace")
	}
	if m.exitCode != 143 {
		t.Fatal("signal exit code lost")
	}
	if cmd := m.workerMessage(signalMsg{code: 143, force: true}); cmd == nil {
		t.Fatal("second signal or expired grace did not force exit")
	}
}

func TestQuitListsBoundedResourceReferencesBeforeForce(t *testing.T) {
	m := fixtureModel(t)
	m.height = 15
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	for _, id := range []string{"task-a", "task-b", "task-c"} {
		_, err := m.tracker.Start(session.Effect, session.Reference{Description: "Dispatch " + id}, func(context.Context) error {
			<-release
			return nil
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	m.beginQuit()
	text := m.pendingText()
	if !strings.Contains(text, "Dispatch task-a") || !strings.Contains(text, "Dispatch task-b") || !strings.Contains(text, "and 1 more") || strings.Contains(text, "Dispatch task-c") {
		t.Fatalf("unbounded or anonymous pending guidance: %q", text)
	}
}

func TestAcceptedContextChangeDropsOldPageCursorAndGeneration(t *testing.T) {
	m := fixtureModel(t)
	m.after, m.cursors, m.generation = "old-project-cursor", []string{"old"}, 8
	m.sections[0].Load = func(_ context.Context, request ui.PageRequest) (ui.Page, error) {
		if request.After != "" {
			t.Error("new project request inherited the old page cursor")
		}
		return ui.Page{}, nil
	}
	m.actionDone(actionMsg{section: 0, feedback: ui.Feedback{ResetPage: true}})
	if m.after != "" || len(m.cursors) != 0 || m.generation != 9 {
		t.Fatal("accepted context retained old cursor/generation")
	}
	m.loaded(loadedMsg{section: 0, generation: 8, page: ui.Page{Summary: "old project"}})
	if m.page.Summary == "old project" {
		t.Fatal("old project load replaced the new context")
	}
}

func TestFinishedEffectAfterRendererStopRetainsReferenceAndCleanupWarning(t *testing.T) {
	m := fixtureModel(t)
	m.runPrepared(ui.Action{ID: "dispatch", Label: "Dispatch", Kind: session.Effect, Reconcile: []string{"delegate", "status", "task-id"}, Run: func(context.Context, app.Arguments) ui.Feedback {
		return ui.Feedback{Err: errors.New("cleanup failed"), RestartRequired: true}
	}}, app.Arguments{TaskID: "task-id"})
	m.beginQuit()
	select {
	case <-m.tracker.Changed():
	case <-time.After(time.Second):
		t.Fatal("effect never settled")
	}
	m.exitCode = 3
	var output bytes.Buffer
	closed := false
	code := finishRun(Options{Error: &output, Close: func() error { closed = true; return nil }}, m, m.tracker, nil, shutdownEvidence{})
	if code != 0 || !closed {
		t.Fatalf("settled force-exit race did not cleanly close: code%d closed%t", code, closed)
	}
	for _, required := range []string{"Dispatch task=task-id", "'delegate' 'status' 'task-id'", "Operation error: cleanup failed", "Restart required:"} {
		if !strings.Contains(output.String(), required) {
			t.Fatalf("post-render disposition lost %q: %s", required, output.String())
		}
	}
	if strings.Contains(output.String(), "Unresolved:") || len(m.tracker.Snapshot().Completed) != 0 {
		t.Fatal("completed effect remained unresolved or unacknowledged")
	}
}

func TestShutdownJoinsCancelledObservationBeforeClosingCache(t *testing.T) {
	m := fixtureModel(t)
	readDone := make(chan struct{})
	_, err := m.tracker.Start(session.Read, session.Reference{Description: "passive task view"}, func(ctx context.Context) error {
		<-ctx.Done()
		close(readDone)
		return ctx.Err()
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.beginQuit()
	var output bytes.Buffer
	closed := false
	code := finishRun(Options{Error: &output, Close: func() error {
		select {
		case <-readDone:
			closed = true
			return nil
		default:
			return errors.New("cache closed before observation joined")
		}
	}}, m, m.tracker, nil, shutdownEvidence{})
	if code != 0 || !closed || output.Len() != 0 {
		t.Fatalf("cancelled observation misreported as uncertain effect: code%d closed%t output%q", code, closed, output.String())
	}
}

func TestClosingDeliveredEffectKeepsErrorForPostTerminalWriter(t *testing.T) {
	m := fixtureModel(t)
	updates := make(chan tea.Msg, 1)
	m.publish = func(msg tea.Msg) { updates <- msg }
	m.runPrepared(ui.Action{ID: "cancel", Label: "Cancel", Kind: session.Effect, Run: func(context.Context, app.Arguments) ui.Feedback {
		return ui.Feedback{Err: errors.New("bounded cancellation fault")}
	}}, app.Arguments{TaskID: "task-id"})
	m.beginQuit()
	select {
	case message := <-updates:
		action, ok := message.(actionMsg)
		if !ok {
			t.Fatalf("completion=%T", message)
		}
		m.actionDone(action)
	case <-time.After(time.Second):
		t.Fatal("completion was not delivered")
	}
	var output bytes.Buffer
	if code := finishRun(Options{Error: &output}, m, m.tracker, nil, shutdownEvidence{}); code != 0 {
		t.Fatalf("settled operation quit code%d", code)
	}
	if strings.Count(output.String(), "Operation returned while closing: Cancel task=task-id") != 1 || !strings.Contains(output.String(), "Operation error: bounded cancellation fault") {
		t.Fatalf("delivered closing disposition was lost or duplicated: %s", output.String())
	}
}

func TestFeedbackActionsNeverTargetThePreviouslySelectedRow(t *testing.T) {
	for _, body := range []string{"Created new-task", ""} {
		t.Run(body, func(t *testing.T) {
			m := fixtureModel(t)
			m.sections[0].Actions = []ui.Action{{ID: "inspect-exact", Label: "Inspect exact ID"}}
			m.page.Entries = []ui.Entry{taskActionEntry("old-task")}
			m.openDetail()
			m.actionDone(actionMsg{epoch: m.navigationEpoch, section: m.selected, feedback: ui.Feedback{Body: body}})
			m.openActions()
			if len(m.actions) != 1 || m.actions[0].ID != "inspect-exact" {
				t.Fatalf("result reused unrelated row actions: %+v", m.actions)
			}
		})
	}
}

func taskActionEntry(id string) ui.Entry {
	entry := ui.Entry{ID: id, Detail: "Task: " + id}
	for _, command := range []string{"cancel", "continue", "collect"} {
		entry.Actions = append(entry.Actions, ui.Action{ID: command, Arguments: app.Arguments{TaskID: id}})
	}
	return entry
}

func assertTaskActionTargets(t *testing.T, actions []ui.Action, want string) {
	t.Helper()
	if len(actions) != 3 {
		t.Fatalf("task action count=%d, want 3", len(actions))
	}
	for _, action := range actions {
		request, err := action.Prepare(nil)
		if err != nil || request.TaskID != want {
			t.Fatalf("%s targets %q, want %q: %v", action.ID, request.TaskID, want, err)
		}
	}
}

func TestDetailActionsKeepOpenedTaskWhenRefreshMovesOrRemovesIt(t *testing.T) {
	for _, retain := range []bool{false, true} {
		t.Run(fmt.Sprint("retain=", retain), func(t *testing.T) {
			m := fixtureModel(t)
			a, b := taskActionEntry(strings.Repeat("a", 32)), taskActionEntry(strings.Repeat("b", 32))
			m.page.Entries = []ui.Entry{a, b}
			m.openDetail()
			snapshot := m.viewport.GetContent()
			entries := []ui.Entry{b}
			if retain {
				entries = append(entries, a)
			}
			m.loaded(loadedMsg{generation: m.generation, section: m.selected, page: ui.Page{Entries: entries}})
			if m.viewport.GetContent() != snapshot || !strings.Contains(snapshot, a.ID) {
				t.Fatal("refresh changed the opened task details")
			}
			m.openActions()
			assertTaskActionTargets(t, m.actions, a.ID)
			m.actionKey("esc")
			m.browseKey(tea.KeyPressMsg{Code: tea.KeyEscape})
			m.openActions()
			listTarget := b.ID
			if retain {
				listTarget = a.ID
			}
			assertTaskActionTargets(t, m.actions, listTarget)
		})
	}
}

func TestGenericDetailAndSectionChangeDoNotRetainTaskActions(t *testing.T) {
	m := fixtureModel(t)
	m.page.Entries = []ui.Entry{taskActionEntry(strings.Repeat("a", 32))}
	m.openDetail()
	m.openText("", "")
	m.openActions()
	if len(m.actions) != 0 {
		t.Fatal("empty generic detail retained row actions")
	}
	m.actionKey("esc")
	m.browseKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m.openDetail()
	m.selectSection(1)
	b := taskActionEntry(strings.Repeat("b", 32))
	m.page.Entries = []ui.Entry{b}
	m.openActions()
	assertTaskActionTargets(t, m.actions, b.ID)
}

func TestAdvancedFieldsStayEditableWithoutChangingHiddenDefaults(t *testing.T) {
	f := newForm(ui.Action{Fields: []ui.Field{
		{Label: "Provider", Value: "exact", Apply: func(a *app.Arguments, s string) error { a.Provider = s; return nil }},
		{Label: "Model", Value: "exact-model", Advanced: true, Apply: func(a *app.Arguments, s string) error { a.Config.Model = s; return nil }},
	}})
	f.move(1)
	if f.focus != 0 {
		t.Fatal("Tab entered hidden field")
	}
	if !f.prepare() || f.prepared.Config.Model != "exact-model" {
		t.Fatal("hidden default lost")
	}
	f.preview = false
	f.toggleAdvanced()
	f.move(1)
	if f.focus != 1 {
		t.Fatal("advanced field unreachable")
	}
}

func TestChangedContextInvalidatesStaleReadsAndBlocksEffects(t *testing.T) {
	m := fixtureModel(t)
	m.generation = 7
	m.invalidateContext()
	if m.generation != 8 || !m.sourceInvalid {
		t.Fatal("context generation not invalidated")
	}
	ran := false
	m.runPrepared(ui.Action{ID: "cancel", Kind: session.Effect, Run: func(context.Context, app.Arguments) ui.Feedback { ran = true; return ui.Feedback{} }}, app.Arguments{})
	if ran || len(m.tracker.Pending()) > 0 {
		t.Fatal("effect started against changed source")
	}
}

func TestLateActionRetainsResultWithoutStealingCurrentView(t *testing.T) {
	m := fixtureModel(t)
	m.selected = 1
	m.navigationEpoch = 9
	m.detail = true
	m.viewport.SetContent("current Team details")
	m.actionDone(actionMsg{key: "old", epoch: 8, section: 0, reference: session.Reference{Description: "old task action"}, feedback: ui.Feedback{Body: "old task completed", RestartRequired: true}})
	if !strings.Contains(m.viewport.View(), "current Team details") {
		t.Fatal("late action replaced current view")
	}
	if len(m.operationResults) != 1 || !strings.Contains(m.operationResults[0], "old task completed") {
		t.Fatal("late result was lost")
	}
	if !m.restartRequired {
		t.Fatal("late cleanup custody failure was ignored")
	}
}

func TestDiagnosticViewDoesNotTargetOldTaskOrAcceptLateResult(t *testing.T) {
	m := fixtureModel(t)
	m.page.Entries = []ui.Entry{{ID: "old", Actions: []ui.Action{{ID: "cancel-old"}}}}
	m.diagnostics = []string{"current diagnostic"}
	epoch := m.navigationEpoch
	m.browseKey(tea.KeyPressMsg{Code: 'd'})
	m.actionDone(actionMsg{epoch: epoch, section: 0, feedback: ui.Feedback{Body: "late action"}})
	if !strings.Contains(m.viewport.View(), "current diagnostic") {
		t.Fatal("late action displaced diagnostic view")
	}
	m.openActions()
	for _, action := range m.actions {
		if action.ID == "cancel-old" {
			t.Fatal("diagnostic view offered an unrelated task mutation")
		}
	}
}

func TestExactRemoteEventsCannotUseCancellableReadContext(t *testing.T) {
	m := fixtureModel(t)
	m.sourceInvalid = true
	action := ui.Action{ID: "events", Kind: session.Read, KindFor: func(a app.Arguments) session.Kind {
		if strings.HasPrefix(a.TaskID, "remote:") {
			return session.Effect
		}
		return session.Read
	}, Run: func(context.Context, app.Arguments) ui.Feedback {
		t.Error("remote write escaped effect guard")
		return ui.Feedback{}
	}}
	m.runPrepared(action, app.Arguments{TaskID: "remote:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	if len(m.tracker.Pending()) != 0 {
		t.Fatal("remote effect started on invalid context")
	}
}

func TestLastRefreshTimerCompletionWakesQuit(t *testing.T) {
	tracker := session.New(t.Context())
	t.Cleanup(tracker.Stop)
	updates := make(chan tea.Msg, 1)
	if err := startTicks(tracker, func(msg tea.Msg) { updates <- msg }); err != nil {
		t.Fatal(err)
	}
	tracker.Stop()
	select {
	case msg := <-updates:
		if _, ok := msg.(finishedMsg); !ok {
			t.Fatalf("timer completion=%T", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("last timer exited without waking the terminal to complete quit")
	}
}

func TestCancelledSearchDoesNotReachRefreshOrReusePageCursor(t *testing.T) {
	m := fixtureModel(t)
	requests := make(chan ui.PageRequest, 1)
	m.sections[0].Load = func(_ context.Context, request ui.PageRequest) (ui.Page, error) {
		requests <- request
		return ui.Page{}, nil
	}
	m.after = "saved-page"
	m.search.SetValue("cancelled edit")
	m.searching = true
	m.searchKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m.refreshTick()
	select {
	case request := <-requests:
		if request.Search != "" || request.After != "saved-page" {
			t.Fatalf("cancelled edit changed active query/page: %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("refresh request not delivered")
	}
}

func TestAppliedSearchResetsPagingAndCancelledDraftRestoresIt(t *testing.T) {
	m := fixtureModel(t)
	requests := make(chan ui.PageRequest, 2)
	m.sections[0].Load = func(_ context.Context, request ui.PageRequest) (ui.Page, error) {
		requests <- request
		return ui.Page{}, nil
	}
	m.after, m.cursors = "old-page", []string{"old"}
	m.search.SetValue("provider publication:committed")
	m.searchKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	select {
	case request := <-requests:
		if request.Search != "provider publication:committed" || request.After != "" || len(m.cursors) != 0 {
			t.Fatalf("applied request=%+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("applied search not loaded")
	}
	m.loading = false
	m.search.SetValue("discarded")
	m.searchKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.search.Value() != m.appliedSearch {
		t.Fatal("cancelled editor does not show active query")
	}
	m.refreshTick()
	select {
	case request := <-requests:
		if request.Search != "provider publication:committed" {
			t.Fatalf("refresh adopted draft: %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("refresh not loaded")
	}
}

func tableModel(t *testing.T) *model {
	t.Helper()
	m := fixtureModel(t)
	m.page.Columns = []ui.Column{{Title: "Task ID", Width: 16}, {Title: "Provider", Width: 18}, {Title: "Admission", Width: 14}, {Title: "Liveness", Width: 14}, {Title: "Publication", Width: 14}, {Title: "Created", Width: 20}}
	m.page.Strip, m.page.Coverage = "Dated · discovery incomplete", "Full coverage: dated observations"
	for _, suffix := range []string{"a", "b"} {
		id := strings.Repeat("a", 31) + suffix
		m.page.Entries = append(m.page.Entries, ui.Entry{ID: id, Cells: []ui.Cell{{Text: id}, {Text: "claude:print"}, {Text: "admitted"}, {Text: "undetermined"}, {Text: "committed"}, {Text: "unknown"}}, Details: []ui.DetailGroup{{Title: "Identity", Fields: []ui.DetailField{{Label: "Task", Value: id}}}}})
	}
	return m
}

func TestTableResizeAndRefreshPreserveFullSelectedIdentity(t *testing.T) {
	m := tableModel(t)
	m.cursor = 1
	selected := m.page.Entries[1].ID
	m.resize(80, 24)
	m.listKey("right")
	if m.tableOffset == 0 {
		t.Fatal("narrow columns did not pan")
	}
	m.resize(170, 40)
	if m.tableOffset != 0 {
		t.Fatal("expanded table retained obsolete pan")
	}
	m.resize(38, 12)
	m.openDetail()
	if !strings.Contains(m.viewport.GetContent(), selected) {
		t.Fatal("details lost full task identity")
	}
	m.detail = false
	m.loaded(loadedMsg{section: 0, generation: m.generation, page: ui.Page{Columns: m.page.Columns, Entries: []ui.Entry{m.page.Entries[1], m.page.Entries[0]}}})
	entry, ok := m.selectedEntry()
	if !ok || entry.ID != selected || m.cursor != 0 {
		t.Fatal("refresh selection followed display position instead of full identity")
	}
}

func TestCoverageIsPassiveAndExcludesRowActions(t *testing.T) {
	m := tableModel(t)
	m.page.Entries[0].Actions = []ui.Action{{ID: "cancel", Label: "Cancel row"}}
	m.browseKey(tea.KeyPressMsg{Code: 'c'})
	if !m.detail || m.message != "History coverage" || !strings.Contains(m.viewport.GetContent(), "dated observations") {
		t.Fatal("coverage view missing")
	}
	m.openActions()
	for _, action := range m.actions {
		if action.ID == "cancel" {
			t.Fatal("coverage inherited selected task action")
		}
	}
}

func TestMinimumFrameKeepsRowsAndBlockingWarnings(t *testing.T) {
	m := tableModel(t)
	m.problem, m.restartRequired, m.sourceInvalid = "refresh failed", true, true
	m.diagnostics, m.operationResults = []string{"diagnostic"}, []string{"result"}
	m.resize(38, 12)
	view := ansi.Strip(m.render())
	if len(strings.Split(view, "\n")) > 12 {
		t.Fatal("frame exceeds terminal height")
	}
	for _, required := range []string{"Task ID", "> ", "SOURCE CHANGED", "RESTART REQUIRED", "refresh failed", "? help", "q quit", "1 diagnostics (d)", "1 results (o)"} {
		if !strings.Contains(view, required) {
			t.Fatalf("minimum frame lost %q: %s", required, view)
		}
	}
	for _, line := range strings.Split(view, "\n") {
		if ansi.StringWidth(line) > 38 {
			t.Fatalf("line exceeds terminal width: %q", line)
		}
	}
}

func TestCompactHeaderKeepsStatusAheadOfLongCoverage(t *testing.T) {
	m := tableModel(t)
	m.diagnostics, m.operationResults = []string{"diagnostic"}, []string{"result"}
	m.page.Next, m.cursors, m.loading = "next-page", []string{"previous-page"}, true
	m.page.Strip = strings.Repeat("Long coverage ", 20)
	m.resize(80, 12)
	lines := strings.Split(ansi.Strip(m.header()), "\n")
	strip := lines[len(lines)-1]
	if !strings.HasPrefix(strip, "1 diagnostics (d) · 1 results (o)") || !strings.Contains(strip, "n next page · p previous page") || !strings.Contains(strip, "Refreshing") {
		t.Fatalf("compact status was hidden behind coverage: %q", strip)
	}
	if strings.Contains(m.footer(), "diagnostics") {
		t.Fatal("compact view reserved a duplicate status row")
	}
}

func TestActionWarningsKeepBottomDetailReachable(t *testing.T) {
	m := fixtureModel(t)
	m.resize(80, 24)
	m.Update(actionMsg{section: 0, feedback: ui.Feedback{Title: "Result", Body: strings.Repeat("line\n", 40) + "BOTTOM-SENTINEL", Err: errors.New("operation failed"), RestartRequired: true}})
	m.viewport.GotoBottom()
	if !strings.Contains(ansi.Strip(m.render()), "BOTTOM-SENTINEL") {
		t.Fatal("new warning rows clipped the viewport's reachable bottom")
	}
}

func TestEverySectionUsesBoundedPaneAndDetailSnapshotSurvivesResize(t *testing.T) {
	m := tableModel(t)
	m.cursor = 1
	id := m.page.Entries[1].ID
	m.resize(170, 40)
	view := ansi.Strip(m.render())
	if !strings.Contains(view, "╭─ Tasks ") || !strings.Contains(view, "┬─ Task details ") {
		t.Fatalf("task split panes missing: %s", view)
	}
	m.openDetail()
	snapshot := m.viewport.GetContent()
	// A new catalog page must not replace the open detail snapshot on resize.
	m.page.Entries[1].Details = []ui.DetailGroup{{Title: "Changed", Fields: []ui.DetailField{{Value: "unrelated refreshed row"}}}}
	for _, width := range []int{38, 80, 120, 170} {
		m.resize(width, 24)
		if m.viewport.GetContent() != snapshot || !strings.Contains(snapshot, id) {
			t.Fatal("resize replaced the selected detail snapshot")
		}
		m.viewport.GotoBottom()
		if !strings.Contains(ansi.Strip(m.render()), "Esc: list") {
			t.Fatal("border displaced full-detail navigation")
		}
	}
	m.detail = false
	m.page = ui.Page{Entries: []ui.Entry{{Title: "Selected record"}}}
	m.cursor = 0
	for i, section := range m.sections {
		m.selected = i
		view = ansi.Strip(m.render())
		if !strings.Contains(view, "╭─ "+section.Name+" ") || !strings.Contains(view, "> Selected record") {
			t.Fatalf("%s uses different pane or selection vocabulary: %s", section.Name, view)
		}
	}
}

func TestFailedSearchLabelsRetainedResults(t *testing.T) {
	m := tableModel(t)
	m.appliedSearch = "admission:not-a-state"
	m.loaded(loadedMsg{generation: m.generation, section: 0, err: errors.New("invalid admission filter")})
	if !strings.Contains(ansi.Strip(m.render()), "Previous results") {
		t.Fatal("failed requested filter mislabeled retained task rows")
	}
}

func TestNonTableViewKeepsMultilineContextAndDiagnostics(t *testing.T) {
	m := fixtureModel(t)
	m.page.Summary = "Optional scoped Teams\nTeam project: /project\nNon-Git root: /parent\nDiagnostic: catalog unavailable"
	m.page.Entries = []ui.Entry{{ID: "team", Title: "Team one"}}
	m.resize(80, 24)
	for _, expected := range strings.Split(m.page.Summary, "\n") {
		if !strings.Contains(ansi.Strip(m.render()), expected) {
			t.Fatalf("view hid context %q", expected)
		}
	}
	if !strings.Contains(ansi.Strip(m.render()), "> Team one") {
		t.Fatal("summary displaced visible row")
	}
}

func TestRetainedSearchPageCannotSupplyAnotherQueryCursor(t *testing.T) {
	m := tableModel(t)
	m.appliedSearch = "invalid:filter"
	m.page.Next = "previous-query-cursor"
	m.cursors = []string{"previous-page"}
	m.listKey("n")
	m.listKey("p")
	if m.after != "" || m.loading {
		t.Fatal("retained page navigated with the requested query")
	}
	if strings.Contains(m.footer(), "next page") {
		t.Fatal("retained page offered invalid pagination")
	}
}
