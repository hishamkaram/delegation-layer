package tasks

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/terminal"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func (v views) result(id string) ui.Action {
	a := v.defaults("status")
	a.TaskID = id
	return exactIDAction(ui.Action{
		ID: "task-result", Label: "Published result", Arguments: a, Kind: session.Read,
		Description:    "Show a bounded prefix only after complete payload validation. This never collects, repairs or indexes provider content.",
		Reconciliation: func(a app.Arguments) []string { return readback(a, "status", a.TaskID) },
		Run: func(ctx context.Context, a app.Arguments) ui.Feedback {
			value, err := v.backend.ReadPublishedResult(ctx, a, taskdir.MaxLogRangeBytes)
			feedback := ui.Feedback{Title: "Published result", Err: err}
			if err == nil {
				feedback.Body = fmt.Sprintf("%d bytes retained of %d verified bytes\n\n%s", len(value.Data), value.TotalBytes, terminal.Safe(string(value.Data)))
				if value.Truncated {
					feedback.Body += "\n\n[Preview truncated; the full published artifact remains with its owner.]"
				}
			}
			return feedback
		},
	}, id)
}

func integerField(label, hint string, value int64, apply func(*app.Arguments, int64)) ui.Field {
	return ui.Field{Label: label, Hint: hint, Value: strconv.FormatInt(value, 10), Apply: func(a *app.Arguments, s string) error {
		parsed, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		apply(a, parsed)
		return nil
	}}
}

func (v views) logRange(id string) ui.Action {
	a := v.defaults("logs")
	a.TaskID, a.LogStream, a.LogLimit = id, "stdout", taskdir.MaxLogRangeBytes
	result := ui.Action{
		ID: "task-log-view", Label: "Open provider log range", Description: "Provider logs may contain private content. Read an explicit bounded range; no following, indexing or implicit collection. A live range is not a final result.", Arguments: a, Kind: session.Read,
		Fields: []ui.Field{
			textField("Stream", "stdout or stderr", "stdout", func(a *app.Arguments, s string) { a.LogStream = s }),
			integerField("Offset", "start byte offset", 0, func(a *app.Arguments, n int64) { a.LogOffset, a.LogOffsetSet = n, true }),
			integerField("Limit", "positive, at most65536 bytes", taskdir.MaxLogRangeBytes, func(a *app.Arguments, n int64) { a.LogLimit = n }),
			textField("Cursor", "optional native object/offset guard from a previous range", "", func(a *app.Arguments, s string) { a.LogCursor = s }),
		}, Reconciliation: func(a app.Arguments) []string { return readback(a, "logs", a.TaskID) },
		Run: func(ctx context.Context, a app.Arguments) ui.Feedback {
			value, err := v.backend.ReadLogRange(ctx, a)
			feedback := ui.Feedback{Title: "Provider log range (private content)", Err: err}
			if err == nil {
				feedback.Body = fmt.Sprintf("%s offset%d • %d/%d bytes • sealed=%t\nCursor: %s\n\n%s", value.Stream, value.Offset, value.Length, value.ObservedSize, value.Sealed, value.Cursor, terminal.Safe(string(value.Data)))
			}
			return feedback
		},
	}
	if id == "" || strings.HasPrefix(id, "remote:") {
		// Origin observation and network/maintenance lifetime are existing
		// remote effects, not a passive local byte-range read.
		result.Kind = session.Effect
	}
	return exactIDAction(result, id)
}
