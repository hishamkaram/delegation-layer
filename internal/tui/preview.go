package tui

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/tui/session"
)

func (m *model) startPreview() {
	f := m.form
	if f == nil || f.action.Preview == nil || f.previewing {
		return
	}
	f.previewing = true
	ctx, cancel := context.WithCancel(m.tracker.ReadContext())
	f.cancelPreview = cancel
	request, version, preview := f.prepared, f.previewVersion, f.action.Preview
	_, err := m.tracker.Start(session.Read, session.Reference{Description: "resolve request preview"}, func(context.Context) error {
		defer cancel()
		value, previewErr := preview(ctx, request)
		m.publish(previewMsg{form: f, version: version, value: value, err: previewErr})
		return nil
	}, m.completed)
	if err != nil {
		f.previewing = false
		f.preview = false
		f.problem = err.Error()
	}
}

func (m *model) previewDone(msg previewMsg) {
	if m.form != msg.form || m.form.previewVersion != msg.version || !m.form.previewing {
		return
	}
	f := m.form
	f.previewing = false
	if msg.err != nil {
		f.preview = false
		f.problem = msg.err.Error()
		return
	}
	f.prepared = msg.value.Arguments
	f.previewSummary = msg.value.Summary
}
