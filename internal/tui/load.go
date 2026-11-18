package tui

import (
	"context"
	"errors"

	"github.com/hishamkaram/delegation-layer/internal/history"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

// A position belongs to the displayed page until a newer load succeeds.
// Treat its back stack as immutable while a loader can hold a copy.
type pagePosition struct {
	after string
	back  []string
}

type pageLoadKind uint8

const (
	loadIdle pageLoadKind = iota
	loadRefresh
	loadPageChange
)

func (m *model) resetPage() {
	m.page = ui.Page{}
	m.pageSearch = ""
	m.position = pagePosition{}
	m.cursor = 0
}

func (m *model) load() {
	if m.pageSearch != m.appliedSearch {
		m.loadPage(pagePosition{}, loadPageChange)
		return
	}
	m.loadPage(m.position, loadRefresh)
}

func (m *model) cancelPageLoad() {
	m.generation++
	m.pageLoad = loadIdle
	if m.cancelLoad != nil {
		m.cancelLoad()
		m.cancelLoad = nil
	}
}

func (m *model) loadPage(position pagePosition, kind pageLoadKind) {
	if m.quitting || len(m.sections) == 0 {
		return
	}
	m.cancelPageLoad()
	generation, section := m.generation, m.selected
	loader := m.sections[section].Load
	if loader == nil {
		return
	}
	ctx, cancel := context.WithCancel(m.tracker.ReadContext())
	m.cancelLoad = cancel
	request := ui.PageRequest{Search: m.appliedSearch, After: position.after}
	m.pageLoad = kind
	_, err := m.tracker.Start(session.Read, session.Reference{Description: "refresh " + m.sections[section].Name}, func(context.Context) error {
		page, loadErr := loader(ctx, request)
		m.publish(loadedMsg{generation: generation, section: section, page: page, search: request.Search, position: position, err: loadErr})
		return nil
	}, m.completed)
	if err != nil {
		m.cancelPageLoad()
		m.problem = err.Error()
	}
}
func (m *model) completed(_ uint64, err error) { m.publish(finishedMsg{err: err}) }
func (m *model) loaded(msg loadedMsg) {
	if msg.generation != m.generation || msg.section != m.selected {
		return
	}
	selectFirst := m.pageLoad == loadPageChange
	m.pageLoad = loadIdle
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
	m.pageSearch = msg.search
	m.position = msg.position
	if selectFirst {
		m.cursor = 0
	} else if hasPrevious {
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
	m.sizeViewport()
}
