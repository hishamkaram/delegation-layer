// Package tui is the explicit terminal frontend. Existing application owners
// remain authoritative for every operation and all durable state.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/terminal"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

type (
	loadedMsg struct {
		generation uint64
		section    int
		page       ui.Page
		err        error
	}
	actionMsg struct {
		operationID uint64
		epoch       uint64
		section     int
		reference   session.Reference
		key         string
		feedback    ui.Feedback
	}
	finishedMsg struct{ err error }
	tickMsg     struct{}
	signalMsg   struct {
		code  int
		force bool
	}
	diagnosticMsg struct {
		text        string
		rootInvalid bool
	}
)

type (
	initMsg    struct{}
	previewMsg struct {
		form    *formState
		version uint64
		value   ui.Preview
		err     error
	}
)

type model struct {
	navigationEpoch        uint64
	operationResults       []string
	checkContext           func(context.Context) error
	onStart                func(*session.Tracker, func(tea.Msg)) error
	sections               []ui.Section
	tracker                *session.Tracker
	publish                func(tea.Msg)
	root, cwd, version     string
	width, height          int
	selected, cursor       int
	generation             uint64
	page                   ui.Page
	loading                bool
	cancelLoad             context.CancelFunc
	search                 textinput.Model
	searching              bool
	after                  string
	cursors                []string
	detail                 bool
	viewport               viewport.Model
	form                   *formState
	actions                []ui.Action
	actionCursor           int
	actionMenu             bool
	message, problem       string
	busy                   map[string]bool
	restartRequired        bool
	sourceInvalid          bool
	quitting, forceConfirm bool
	exitCode               int
	showHelp               bool
	diagnostics            []string
}

func newModel(sections []ui.Section, root, cwd, version string, tracker *session.Tracker, publish func(tea.Msg)) *model {
	search := textinput.New()
	search.Placeholder = "Search metadata across history"
	search.CharLimit = 1024
	return &model{sections: sections, root: root, cwd: cwd, version: version, tracker: tracker, publish: publish, search: search, viewport: viewport.New(), busy: make(map[string]bool)}
}

func (m *model) Init() tea.Cmd { return func() tea.Msg { return initMsg{} } }
func (m *model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
	case tea.KeyPressMsg:
		_, cmd = m.handleKey(msg)
	default:
		cmd = m.workerMessage(message)
	}
	if m.quitting && len(m.tracker.Pending()) == 0 {
		return m, tea.Quit
	}
	return m, cmd
}

func (m *model) workerMessage(message tea.Msg) tea.Cmd {
	switch msg := message.(type) {
	case initMsg:
		m.initialize()
	case loadedMsg:
		m.loaded(msg)
	case actionMsg:
		m.actionDone(msg)
	case previewMsg:
		m.previewDone(msg)
	case finishedMsg:
		m.workerFinished(msg.err)
	case tickMsg:
		m.refreshTick()
	case signalMsg:
		m.exitCode = msg.code
		if msg.force {
			return tea.Quit
		}
		m.beginQuit()
	case diagnosticMsg:
		m.addDiagnostic(msg)
	default:
		return m.inputMessage(message)
	}
	return nil
}

func (m *model) workerFinished(err error) {
	if err != nil && !m.quitting {
		m.problem = err.Error()
	}
}

func (m *model) refreshTick() {
	if m.sourceInvalid {
		return
	}
	if !m.quitting && m.form == nil && !m.actionMenu && !m.searching && !m.detail && !m.loading {
		m.load()
	}
}

func (m *model) addDiagnostic(msg diagnosticMsg) {
	if msg.rootInvalid {
		m.invalidateContext()
	}
	text := msg.text
	m.diagnostics = append(m.diagnostics, text)
	if len(m.diagnostics) > 50 {
		m.diagnostics = m.diagnostics[len(m.diagnostics)-50:]
	}
}

func (m *model) resize(width, height int) {
	m.width, m.height = width, height
	m.search.SetWidth(max(10, width-5))
	m.viewport.SetWidth(max(10, width-8))
	m.viewport.SetHeight(max(3, height-9))
}

func (m *model) beginQuit() {
	m.quitting = true
	m.form = nil
	m.actionMenu = false
	if m.cancelLoad != nil {
		m.cancelLoad()
	}
	m.tracker.Stop()
}

func (m *model) selectedEntry() (ui.Entry, bool) {
	if m.cursor < 0 || m.cursor >= len(m.page.Entries) {
		return ui.Entry{}, false
	}
	return m.page.Entries[m.cursor], true
}

func (m *model) selectSection(index int) {
	if index < 0 || index >= len(m.sections) {
		return
	}
	if m.cancelLoad != nil {
		m.cancelLoad()
	}
	m.navigationEpoch++
	m.selected = index
	m.cursor = 0
	m.after = ""
	m.cursors = nil
	m.detail = false
	m.problem = ""
	m.generation++
	m.loading = false
	m.page = ui.Page{}
	m.load()
}

func (m *model) openDetail() {
	m.navigationEpoch++
	if entry, ok := m.selectedEntry(); ok {
		m.detail = true
		m.message = ""
		m.viewport.SetContent(terminal.Safe(entry.Detail))
		m.viewport.GotoTop()
	}
}

func (m *model) openActions() {
	m.navigationEpoch++
	m.actions = append([]ui.Action(nil), m.sections[m.selected].Actions...)
	m.actions = append(m.actions, m.page.Actions...)
	if entry, ok := m.selectedEntry(); ok && (!m.detail || m.message == "") {
		m.actions = append(m.actions, entry.Actions...)
	}
	m.actionCursor = 0
	m.actionMenu = true
}

func (m *model) actionDone(msg actionMsg) {
	delete(m.busy, msg.key)
	m.applyDisposition(msg)
	if m.quitting {
		// The tracker keeps the full Effect disposition until the single
		// post-terminal writer reports it, including errors and cleanup custody.
		return
	}
	body := msg.feedback.Body
	if len(body) > 64<<10 {
		body = body[:64<<10] + "\n[Display limited to 64 KiB]"
	}
	record := msg.reference.Description + "\n" + body
	if msg.feedback.Err != nil {
		record += "\nError: " + msg.feedback.Err.Error()
	}
	m.operationResults = append(m.operationResults, record)
	if len(m.operationResults) > 50 {
		m.operationResults = m.operationResults[len(m.operationResults)-50:]
	}
	m.tracker.Acknowledge(msg.operationID)
	if msg.epoch != m.navigationEpoch || msg.section != m.selected || m.form != nil || m.actionMenu || m.searching || m.showHelp {
		return
	}
	m.message = body
	if msg.feedback.Err != nil {
		m.problem = msg.feedback.Err.Error()
	} else {
		m.problem = ""
	}
	m.viewport.SetContent(terminal.Safe(m.message))
	m.viewport.GotoTop()
	m.detail = true
}

func (m *model) applyDisposition(msg actionMsg) {
	if msg.feedback.ContextInvalid {
		m.invalidateContext()
	}
	if msg.feedback.RestartRequired {
		m.restartRequired = true
	}
	if msg.feedback.ResetPage && msg.section == m.selected && !m.quitting {
		m.generation++
		m.after = ""
		m.cursors = nil
		m.loading = false
		m.load()
	}
}

func (m *model) startAction(action ui.Action, requestValues []string) {
	request, err := action.Prepare(requestValues)
	if err != nil {
		m.problem = err.Error()
		return
	}
	m.runPrepared(action, request)
}

func (m *model) pendingText() string {
	pending := m.tracker.Pending()
	var text strings.Builder
	fmt.Fprintf(&text, "Waiting for %d session operation(s). f: force-exit confirmation\n", len(pending))
	visible := min(len(pending), max(1, m.height-13))
	for _, operation := range pending[:visible] {
		fmt.Fprintf(&text, "\n• %s", oneLine(operation.Reference.Description, m.width-6))
	}
	if len(pending) > visible {
		fmt.Fprintf(&text, "\n… and %d more; force exit prints all reconciliation references", len(pending)-visible)
	}
	return text.String()
}

// StartTicks is a session-owned observation worker, rather than an unjoined
// renderer command. Its timer and worker both close when reads are cancelled.
func startTicks(tracker *session.Tracker, publish func(tea.Msg)) error {
	_, err := tracker.Start(session.Read, session.Reference{Description: "view refresh timer"}, func(ctx context.Context) error {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				publish(tickMsg{})
			}
		}
	}, func(_ uint64, err error) { publish(finishedMsg{err: err}) })
	return err
}

func (m *model) initialize() {
	if m.quitting {
		return
	}
	if err := startTicks(m.tracker, m.publish); err != nil {
		m.problem = err.Error()
	}
	if m.onStart != nil {
		if err := m.onStart(m.tracker, m.publish); err != nil {
			m.problem = err.Error()
		}
	}
	m.load()
}

func (m *model) inputMessage(message tea.Msg) tea.Cmd {
	if m.form != nil && !m.form.preview {
		return m.form.update(message)
	}
	if m.searching {
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(message)
		return cmd
	}
	return nil
}

func (m *model) invalidateContext() {
	if m.sourceInvalid {
		return
	}
	m.sourceInvalid = true
	m.generation++
	m.loading = false
	if m.cancelLoad != nil {
		m.cancelLoad()
	}
}
