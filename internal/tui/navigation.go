package tui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/terminal"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func (m *model) handleKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.quitting {
		return m, m.quitKey(key.String())
	}
	if key.String() == "ctrl+c" {
		m.beginQuit()
		return m, nil
	}
	if m.form != nil {
		return m, m.formKey(key)
	}
	if m.searching {
		return m, m.searchKey(key)
	}
	if m.actionMenu {
		return m, m.actionKey(key.String())
	}
	if m.showHelp {
		m.showHelp = false
		return m, nil
	}
	return m, m.browseKey(key)
}

func (m *model) quitKey(key string) tea.Cmd {
	switch key {
	case "f":
		m.forceConfirm = true
	case "esc":
		m.forceConfirm = false
	case "enter":
		if m.forceConfirm {
			m.exitCode = 3
			return tea.Quit
		}
	}
	return nil
}

func (m *model) formKey(key tea.KeyPressMsg) tea.Cmd {
	switch key.String() {
	case "esc":
		if m.form.preview {
			if m.form.cancelPreview != nil {
				m.form.cancelPreview()
			}
			m.form.previewVersion++
			m.form.previewing = false
			m.form.preview = false
		} else {
			m.form = nil
		}
		return nil
	case "enter":
		if m.form.preview && !m.form.previewing {
			action, request := m.form.action, m.form.prepared
			m.form = nil
			m.runPrepared(action, request)
			return nil
		}
	}
	if m.form.previewing {
		return nil
	}
	if m.form.preview {
		var cmd tea.Cmd
		m.form.previewViewport, cmd = m.form.previewViewport.Update(key)
		return cmd
	}
	cmd := m.form.update(key)
	if m.form.preview {
		m.startPreview()
	}
	return cmd
}

func (m *model) searchKey(key tea.KeyPressMsg) tea.Cmd {
	switch key.String() {
	case "esc":
		m.searching = false
		m.search.Blur()
		return nil
	case "enter":
		m.navigationEpoch++
		m.searching = false
		m.search.Blur()
		m.after = ""
		m.cursors = nil
		m.generation++
		m.loading = false
		m.load()
		return nil
	}
	var cmd tea.Cmd
	m.search, cmd = m.search.Update(key)
	return cmd
}

func (m *model) actionKey(key string) tea.Cmd {
	switch key {
	case "esc":
		m.actionMenu = false
	case "up", "k":
		m.actionCursor = max(0, m.actionCursor-1)
	case "down", "j":
		m.actionCursor = min(max(0, len(m.actions)-1), m.actionCursor+1)
	case "enter":
		if len(m.actions) == 0 {
			return nil
		}
		action := m.actions[m.actionCursor]
		if action.Disabled != "" {
			m.problem = action.Disabled
			return nil
		}
		m.actionMenu = false
		if len(action.Fields) > 0 || action.Confirm {
			m.form = newForm(action)
			if len(action.Fields) == 0 {
				m.form.prepare()
			}
			return m.form.focusField()
		}
		m.startAction(action, nil)
	}
	return nil
}

func (m *model) browseKey(key tea.KeyPressMsg) tea.Cmd {
	switch key.String() {
	case "q":
		m.beginQuit()
	case "?":
		m.showHelp = true
	case "tab":
		m.selectSection((m.selected + 1) % len(m.sections))
	case "shift+tab":
		m.selectSection((m.selected + len(m.sections) - 1) % len(m.sections))
	case "1", "2", "3", "4":
		m.selectSection(int(key.String()[0] - '1'))
	case "/":
		m.searching = true
		return m.search.Focus()
	case "r":
		m.generation++
		m.loading = false
		m.load()
	case "a":
		m.openActions()
	case "o":
		m.navigationEpoch++
		m.viewport.SetContent(terminal.Safe(strings.Join(m.operationResults, "\n\n────\n\n")))
		m.viewport.GotoTop()
		m.message = "Operation results"
		m.detail = true
	case "d":
		m.navigationEpoch++
		m.message = "Diagnostics"
		m.viewport.SetContent(strings.Join(m.diagnostics, "\n\n"))
		m.viewport.GotoTop()
		m.detail = true
	case "enter":
		m.openDetail()
	case "esc":
		m.navigationEpoch++
		m.detail = false
		m.message = ""

	}
	if m.listKey(key.String()) {
		return nil
	}
	if m.detail {
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(key)
		return cmd
	}
	return nil
}

func (m *model) runPrepared(action ui.Action, request app.Arguments) {
	kind := action.Kind
	if action.KindFor != nil {
		kind = action.KindFor(request)
	}
	key := fmt.Sprintf("%q", []string{action.ID, request.Root, request.TaskID, request.ResumeTask, request.ScheduleID, request.WorkspaceID, request.CheckpointID, request.TeamScope, request.TeamID, request.ProjectRoot})
	if m.busy[key] {
		m.problem = "This operation is already in flight"
		return
	}
	if kind == session.Effect && m.sourceInvalid {
		m.problem = "Source root binding changed; reopen and validate the context before another effect"
		return
	}
	if kind == session.Effect && m.restartRequired {
		m.problem = "This state root requires restart after unresolved cleanup; new effects are blocked"
		return
	}
	reconcile := action.Reconcile
	if action.Reconciliation != nil {
		reconcile = action.Reconciliation(request)
	}
	reference := session.Reference{Description: actionReference(action.Label, request), Command: reconcile}
	epoch, section := m.navigationEpoch, m.selected
	m.busy[key] = true
	var feedback ui.Feedback
	work := func(ctx context.Context) session.Disposition {
		if kind == session.Effect && m.checkContext != nil {
			if err := m.checkContext(ctx); err != nil {
				feedback = ui.Feedback{Err: err, Body: err.Error(), ContextInvalid: true}
				return session.Disposition{Err: err}
			}
		}
		feedback = action.Run(ctx, request)
		return session.Disposition{Err: feedback.Err, RestartRequired: feedback.RestartRequired}
	}
	complete := func(id uint64, err error) {
		if err != nil && feedback.Err == nil {
			feedback.Err = err
		}
		m.publish(actionMsg{operationID: id, epoch: epoch, section: section, reference: reference, key: key, feedback: feedback})
	}
	var err error
	if kind == session.Effect {
		_, err = m.tracker.StartEffect(reference, work, complete)
	} else {
		_, err = m.tracker.Start(kind, reference, func(ctx context.Context) error { return work(ctx).Err }, complete)
	}
	if err != nil {
		delete(m.busy, key)
		m.problem = err.Error()
	}
}

func (m *model) listKey(key string) bool {
	if m.detail {
		return false
	}
	switch key {
	case "up", "k":
		m.navigationEpoch++
		m.cursor = max(0, m.cursor-1)
	case "down", "j":
		m.navigationEpoch++
		m.cursor = min(max(0, len(m.page.Entries)-1), m.cursor+1)
	case "n":
		if m.page.Next != "" {
			m.cursors = append(m.cursors, m.after)
			m.after = m.page.Next
			m.pageChanged()
		}
	case "p":
		if len(m.cursors) > 0 {
			m.after = m.cursors[len(m.cursors)-1]
			m.cursors = m.cursors[:len(m.cursors)-1]
			m.pageChanged()
		}
	default:
		return false
	}
	return true
}

func (m *model) pageChanged() {
	m.navigationEpoch++
	m.cursor = 0
	m.generation++
	m.loading = false
	m.load()
}

func actionReference(label string, a app.Arguments) string {
	for _, item := range []struct{ name, value string }{{"task", a.TaskID}, {"successor", a.SuccessorTaskID}, {"message", a.MessageID}, {"schedule", a.ScheduleID}, {"workspace", a.WorkspaceID}, {"checkpoint", a.CheckpointID}, {"team", a.TeamID}, {"scope", a.TeamScope}, {"device", a.Device}} {
		if item.value != "" {
			label += " " + item.name + "=" + item.value
		}
	}
	return label
}
