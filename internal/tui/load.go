package tui

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/history"

	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func (m *model) load() {
	if m.loading || m.quitting || len(m.sections) == 0 {
		return
	}
	if m.cancelLoad != nil {
		m.cancelLoad()
	}
	ctx, cancel := context.WithCancel(m.tracker.ReadContext())
	m.cancelLoad = cancel
	generation, section := m.generation, m.selected
	loader := m.sections[section].Load
	if loader == nil {
		return
	}
	request := ui.PageRequest{Search: m.search.Value(), After: m.after}
	m.loading = true
	_, err := m.tracker.Start(session.Read, session.Reference{Description: "refresh " + m.sections[section].Name}, func(context.Context) error {
		page, loadErr := loader(ctx, request)
		m.publish(loadedMsg{generation: generation, section: section, page: page, err: loadErr})
		return nil
	}, m.completed)
	if err != nil {
		m.loading = false
		m.problem = err.Error()
	}
}
func (m *model) completed(_ uint64, err error) { m.publish(finishedMsg{err: err}) }
func (m *model) loaded(msg loadedMsg) {
	if msg.generation != m.generation || msg.section != m.selected {
		return
	}
	m.loading = false
	if errors.Is(msg.err, context.Canceled) {
		return
	}
	if msg.err != nil {
		if errors.Is(msg.err, history.ErrRootChanged) {
			m.invalidateContext()
		}
		m.problem = msg.err.Error()
		return
	}
	previous, hasPrevious := m.selectedEntry()
	m.page = msg.page
	if hasPrevious {
		for i, entry := range m.page.Entries {
			if entry.ID == previous.ID {
				m.cursor = i
				break
			}
		}
	}
	if m.cursor >= len(m.page.Entries) {
		m.cursor = max(0, len(m.page.Entries)-1)
	}
	m.problem = ""
}
