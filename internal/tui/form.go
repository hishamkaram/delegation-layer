package tui

import (
	"context"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/tui/terminal"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

type fieldEditor struct {
	single    textinput.Model
	multi     textarea.Model
	multiline bool
}

func newEditor(field ui.Field) fieldEditor {
	editor := fieldEditor{multiline: field.Multiline}
	if field.Multiline {
		editor.multi = textarea.New()
		editor.multi.SetValue(field.Value)
		editor.multi.SetHeight(4)
		editor.multi.CharLimit = 1024 * 1024
	} else {
		editor.single = textinput.New()
		editor.single.SetValue(field.Value)
		editor.single.CharLimit = 8192
	}
	return editor
}

func (e *fieldEditor) focus() tea.Cmd {
	if e.multiline {
		return e.multi.Focus()
	}
	return e.single.Focus()
}

func (e *fieldEditor) blur() {
	if e.multiline {
		e.multi.Blur()
	} else {
		e.single.Blur()
	}
}

func (e fieldEditor) value() string {
	if e.multiline {
		return e.multi.Value()
	}
	return e.single.Value()
}

func (e *fieldEditor) update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	if e.multiline {
		e.multi, cmd = e.multi.Update(msg)
	} else {
		e.single, cmd = e.single.Update(msg)
	}
	return cmd
}

func (e *fieldEditor) view(width int) string {
	if e.multiline {
		e.multi.SetWidth(max(12, width))
		return e.multi.View()
	}
	e.single.SetWidth(max(12, width))
	return e.single.View()
}

type formState struct {
	showAdvanced    bool
	previewing      bool
	previewVersion  uint64
	previewSummary  string
	cancelPreview   context.CancelFunc
	previewViewport viewport.Model
	action          ui.Action
	editors         []fieldEditor
	focus           int
	preview         bool
	prepared        app.Arguments
	problem         string
}

func newForm(action ui.Action) *formState {
	f := &formState{action: action, previewViewport: viewport.New()}
	f.previewViewport.SoftWrap = true
	for _, field := range action.Fields {
		f.editors = append(f.editors, newEditor(field))
	}
	return f
}

func (f *formState) focusField() tea.Cmd {
	if len(f.editors) == 0 {
		return nil
	}
	return f.editors[f.focus].focus()
}

func (f *formState) visibleFields() []int {
	var indices []int
	for i, field := range f.action.Fields {
		if !field.Advanced || f.showAdvanced {
			indices = append(indices, i)
		}
	}
	return indices
}

func (f *formState) move(delta int) tea.Cmd {
	visible := f.visibleFields()
	if len(visible) == 0 {
		return nil
	}
	current := 0
	for i, index := range visible {
		if index == f.focus {
			current = i
			break
		}
	}
	f.editors[f.focus].blur()
	f.focus = visible[(current+delta+len(visible))%len(visible)]
	return f.focusField()
}

func (f *formState) toggleAdvanced() tea.Cmd {
	f.showAdvanced = !f.showAdvanced
	if !f.showAdvanced && f.action.Fields[f.focus].Advanced {
		f.editors[f.focus].blur()
		visible := f.visibleFields()
		if len(visible) > 0 {
			f.focus = visible[0]
		}
	}
	return f.focusField()
}

func (f *formState) prepare() bool {
	values := make([]string, len(f.editors))
	for i, e := range f.editors {
		values[i] = e.value()
	}
	a, err := f.action.Prepare(values)
	if err != nil {
		f.problem = err.Error()
		return false
	}
	f.prepared = a
	f.previewVersion++
	f.previewSummary = ""
	f.preview = true
	f.previewViewport.GotoTop()
	f.problem = ""
	return true
}

func (f *formState) update(msg tea.Msg) tea.Cmd {
	key, ok := msg.(tea.KeyPressMsg)
	if ok {
		switch key.String() {
		case "tab":
			return f.move(1)
		case "shift+tab":
			return f.move(-1)
		case "ctrl+o":
			return f.toggleAdvanced()
		case "ctrl+s":
			f.prepare()
			return nil
		}
	}
	if len(f.editors) == 0 {
		return nil
	}
	return f.editors[f.focus].update(msg)
}

func (f *formState) view(width, height int) string {
	if f.preview {
		return f.previewView(width, height)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%s\n%s\n\n", terminal.Safe(f.action.Label), terminal.Safe(f.action.Description))
	// A window around focus keeps long forms usable without moving focus as
	// asynchronous data arrives. Input widgets own cursor and text editing.
	visible := f.visibleFields()
	position := 0
	for i, index := range visible {
		if index == f.focus {
			position = i
			break
		}
	}
	start := max(0, position-max(0, (height-12)/5))
	remaining := max(4, height-9)
	for _, i := range visible[start:] {
		if remaining <= 2 {
			break
		}
		label := terminal.Safe(f.action.Fields[i].Label)
		marker := "  "
		if i == f.focus {
			marker = "> "
		}
		fmt.Fprintf(&out, "%s%s\n%s\n", marker, label, f.editors[i].view(width-6))
		fmt.Fprintln(&out, terminal.Safe(f.action.Fields[i].Hint))
		if f.editors[i].multiline {
			remaining -= 7
		} else {
			remaining -= 4
		}
	}

	if f.problem != "" {
		fmt.Fprintf(&out, "\nError: %s", terminal.Safe(f.problem))
	}
	out.WriteString("\nTab next · Ctrl-O advanced options · Ctrl-S review · Esc back")
	return out.String()
}

func (f *formState) previewView(width, height int) string {
	var out strings.Builder
	if f.previewing {
		return "Resolving the effective request from existing configuration…\nEsc back · Ctrl-C quit"
	}
	fmt.Fprintf(&out, "Review: %s\n\n%s\n", terminal.Safe(f.action.Label), terminal.Safe(f.action.Description))
	if f.previewSummary != "" {
		fmt.Fprintln(&out, terminal.Safe(f.previewSummary))
	}
	a := f.prepared
	for _, item := range []struct{ name, value string }{
		{"State root", a.Root}, {"Workspace", a.Cwd}, {"Task", a.TaskID}, {"Successor", a.SuccessorTaskID}, {"Message", a.MessageID}, {"Predecessor", a.ResumeTask}, {"Schedule", a.ScheduleID}, {"Workspace ID", a.WorkspaceID}, {"Checkpoint", a.CheckpointID}, {"Provider", a.Provider}, {"Model (requested)", a.Config.Model}, {"Effort", a.Config.Effort}, {"Permission", a.Config.Permission}, {"Budget", a.Config.Budget}, {"Output-silence timeout", a.Config.OutputSilenceTimeout}, {"Remote device", a.Device}, {"Remote root", a.RemoteRoot}, {"Remote scope", a.RemoteScope}, {"Team scope", a.TeamScope}, {"Project root", a.ProjectRoot}, {"Team", a.TeamID}, {"Member", a.MemberID},
	} {
		if item.value != "" {
			fmt.Fprintf(&out, "%s: %s\n", item.name, terminal.Safe(item.value))
		}
	}
	if a.TeamExpectedRevision != 0 {
		fmt.Fprintf(&out, "Expected Team revision: %d\n", a.TeamExpectedRevision)
	}
	for i, field := range f.action.Fields {
		value := terminal.Safe(f.editors[i].value())
		if len(value) > 512 {
			value = value[:512] + "… [preview truncated]"
		}
		fmt.Fprintf(&out, "\n%s: %s\n", terminal.Safe(field.Label), value)
	}
	if f.action.Confirm {
		out.WriteString("\nThis action changes state. Confirm the resource and scope above.\n")
	}
	f.previewViewport.SetWidth(max(12, width))
	f.previewViewport.SetHeight(max(3, height-3))
	f.previewViewport.SetContent(out.String())
	return f.previewViewport.View() + "\n↑↓ scroll · Enter confirm · Esc edit · Ctrl-C quit"
}
