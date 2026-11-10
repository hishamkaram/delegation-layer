package tasks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/app"
	"github.com/hishamkaram/delegation-layer/internal/history"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func prepare(t *testing.T, action ui.Action, edits map[string]string) app.Arguments {
	t.Helper()
	values := make([]string, len(action.Fields))
	for i, field := range action.Fields {
		values[i] = field.Value
		if value, found := edits[field.Label]; found {
			values[i] = value
			delete(edits, field.Label)
		}
	}
	if len(edits) != 0 {
		t.Fatalf("unknown form edits: %v", edits)
	}
	a, err := action.Prepare(values)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestTaskDispatchFormsPreserveExplicitSelectionAndCaptureReferences(t *testing.T) {
	v := views{base: app.Arguments{Root: t.TempDir()}, cwd: t.TempDir()}
	action := v.dispatch(false)

	a := prepare(t, action, map[string]string{"Provider": "codex:exec", "Requested model": "exact-requested-id", "Effort": "high", "Permission": "read-only", "Budget": "12m", "Brief text": "bounded instruction"})
	got := []string{a.Root, a.Cwd, a.Provider, a.Config.Model, a.Config.Effort, a.Config.Permission, a.Config.Budget, string(a.BriefBytes)}
	want := []string{v.base.Root, v.cwd, "codex:exec", "exact-requested-id", "high", "read-only", "12m", "bounded instruction"}
	if !slices.Equal(got, want) {
		t.Fatalf("form silently retargeted the request: %+v", a)
	}
	if a.ResourceGuards.DisableCPU || a.ResourceGuards.DisableMemory {
		t.Fatal("guards were not on by default")
	}
	if err := task.ValidateTaskID(a.TaskID); err != nil {
		t.Fatal(err)
	}
	reconcile := action.Reconciliation(a)
	if len(reconcile) < 5 || reconcile[1] != "--root" || reconcile[2] != v.base.Root || reconcile[4] != a.TaskID {
		t.Fatalf("ambiguous reconciliation context: %v", reconcile)
	}
}

func TestTaskDispatchKeepsBasicFieldsVisible(t *testing.T) {
	v := views{base: app.Arguments{Root: t.TempDir()}, cwd: t.TempDir()}
	action := v.dispatch(false)
	for _, field := range action.Fields {
		if field.Label == "Requested model" && !field.Advanced {
			t.Fatal("advanced model selection crowds the basic form")
		}
		if (field.Label == "Permission" || field.Label == "Budget" || field.Label == "Brief text") && field.Advanced {
			t.Fatalf("basic field hidden: %s", field.Label)
		}
	}
}

func TestRemoteDispatchNeverInheritsLocalSelection(t *testing.T) {
	v := views{base: app.Arguments{Root: t.TempDir()}, cwd: t.TempDir()}
	remote := v.dispatch(true)
	r := prepare(t, remote, map[string]string{"Provider": "claude:print", "SSH device": "chosen-device", "Destination root": "/destination/state", "Destination working directory": "/destination/work", "Brief text": "remote instruction"})
	if r.Cwd != "" || r.Auto || r.TeamID != "" || r.Device != "chosen-device" || r.RemoteRoot != "/destination/state" || r.RemoteCwd != "/destination/work" {
		t.Fatalf("remote form inherited unsupported local selection: %+v", r)
	}
	if remote.Reconciliation(r)[4] != "remote:"+r.TaskID {
		t.Fatal("remote reference lost during reconciliation")
	}
}

func TestTaskContinuationDoesNotBroadenPermissionOrLoseSuccessorIdentity(t *testing.T) {
	v := views{base: app.Arguments{Root: t.TempDir()}}
	id := strings.Repeat("a", 32)
	action := v.continueTask(id)
	for _, field := range action.Fields {
		if field.Label == "Permission" || field.Label == "Provider" || field.Label == "Working directory" {
			t.Fatalf("continuation offers an unsupported override: %s", field.Label)
		}
	}
	a := prepare(t, action, map[string]string{"Budget": "20m"})
	if a.TaskID != id || a.SuccessorTaskID == id || a.Config.Permission != "" {
		t.Fatalf("continuation changed inherited authority: %+v", a)
	}
	if err := task.ValidateTaskID(a.SuccessorTaskID); err != nil {
		t.Fatal(err)
	}
	if action.Reconciliation(a)[4] != a.SuccessorTaskID {
		t.Fatal("successor reconciliation targets the predecessor")
	}
}

func TestTaskUnavailableHistoryKeepsExactIDActionsWithoutDuplicates(t *testing.T) {
	section := NewSection(nil, app.Arguments{Root: t.TempDir()}, t.TempDir(), nil)
	page, err := section.Load(t.Context(), ui.PageRequest{})
	if err != nil || !strings.Contains(page.Summary, "Exact-ID actions remain available") || len(page.Entries) != 0 || len(page.Actions) != 0 || len(page.Columns) != 0 {
		t.Fatalf("degraded history misrepresented global results: %+v, %v", page, err)
	}
	found := map[string]bool{}
	for _, action := range section.Actions {
		if found[action.ID] {
			t.Fatalf("duplicate global action: %s", action.ID)
		}
		found[action.ID] = true
	}
	if !found["task-status"] || !found["task-collect"] || !found["task-cancel"] {
		t.Fatal("history failure hid independent exact-ID controls")
	}
}

func TestTaskSearchFiltersAreExplicitDatedFields(t *testing.T) {
	query, err := searchQuery("codex:exec publication:committed liveness:undetermined")
	if err != nil || query.Search != "codex:exec" || query.Publication != "committed" || query.Liveness != "undetermined" {
		t.Fatalf("wrong query binding: %+v %v", query, err)
	}
	query, err = searchQuery("publication")
	if err != nil || query.Search != "publication" {
		t.Fatal("ordinary metadata word was incorrectly treated as a filter")
	}
	for _, input := range []string{"publication:", "status:x status:y"} {
		if _, err = searchQuery(input); err == nil {
			t.Fatalf("ambiguous filter accepted: %s", input)
		}
	}
}

func TestTaskEntryPreservesIndependentEvidenceAndUnsupportedRemoteActions(t *testing.T) {
	v := views{base: app.Arguments{Root: t.TempDir()}}
	row := history.Row{
		Identity: history.Identity{Kind: taskdir.IndexRemote, TaskID: strings.Repeat("a", 32)}, Metadata: taskdir.IndexMetadata{Provider: "fixture:test"},
		Observation: &history.Observation{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Admission: task.AdmissionAdmitted, Liveness: task.LivenessUndetermined, Publication: task.PublicationCommitted, Source: "origin-cache"},
	}
	entry := v.entry(row)
	want := map[string]string{"Identity/Created": "unknown", "Evidence/Admission": "admitted", "Evidence/Liveness": "undetermined", "Evidence/Publication": "committed", "Evidence/Source": "origin-cache"}
	for field, value := range want {
		if got := entryFields(entry)[field]; got != value {
			t.Fatalf("lost evidence %s: got %q, want %q", field, got, value)
		}
	}
	wantCells := []ui.Cell{{Text: row.Handle()}, {Text: "fixture:test"}, {Text: "admitted", Role: ui.RolePositive}, {Text: "undetermined"}, {Text: "committed", Role: ui.RolePositive}, {Text: "unknown"}}
	if !slices.Equal(entry.Cells, wantCells) {
		t.Fatalf("table merged independent evidence: %+v", entry.Cells)
	}
	if entry.ID != row.Handle() || entryFields(entry)["Identity/Task"] != row.Handle() || entry.Detail != "" {
		t.Fatalf("task identity or structured detail authority changed: %+v", entry)
	}
	for _, action := range entry.Actions {
		if (action.ID == "task-continue" || action.ID == "task-recheck") && action.Disabled == "" {
			t.Fatalf("unsupported remote action enabled: %s", action.ID)
		}
		if action.ID == "task-status" && action.Kind != session.Effect {
			t.Fatal("write-capable Inspect was classified as a cancellable passive read")
		}
	}
}

func entryFields(entry ui.Entry) map[string]string {
	fields := map[string]string{}
	for _, group := range entry.Details {
		for _, field := range group.Fields {
			fields[group.Title+"/"+field.Label] = field.Value
		}
	}
	return fields
}

func TestTaskEntryDistinguishesAbsentObservationAndRetainedFaultedEvidence(t *testing.T) {
	v := views{}
	row := history.Row{Identity: history.Identity{Kind: taskdir.IndexLocal, TaskID: strings.Repeat("a", 32)}, Metadata: taskdir.IndexMetadata{
		Provider: "claude:print", Model: "selected", Effort: "high", Cwd: "/chosen/work", WorkspaceID: "workspace", TeamID: "team", MemberID: "member",
		CreatedAtKnown: true, CreatedAt: time.Date(2026, 1, 2, 10, 0, 0, 0, time.FixedZone("fixture", 7*60*60)),
	}}
	entry := v.entry(row)
	for _, cell := range entry.Cells[2:5] {
		if cell.Text != "not observed" || cell.Fault || cell.Role != ui.RoleNeutral {
			t.Fatalf("missing observation became an observed enum: %+v", entry.Cells)
		}
	}
	if entry.Cells[5].Text != "2026-01-02T03:00:00Z" || entryFields(entry)["Identity/Created"] != "2026-01-02T03:00:00Z" {
		t.Fatalf("known creation instant was not retained in UTC: %+v", entry)
	}
	for field, value := range map[string]string{"Provider": "claude:print", "Requested model": "selected", "Requested effort": "high", "Working directory": "/chosen/work", "Workspace": "workspace", "Team/member": "team/member"} {
		if got := entryFields(entry)["Request/context/"+field]; got != value {
			t.Fatalf("lost request/context %s: got %q want %q", field, got, value)
		}
	}
	row.Observation = &history.Observation{Admission: task.AdmissionUnknown, Liveness: task.LivenessUndetermined, Publication: task.PublicationPending, Status: "blocked", FailureCode: "fixture-failure", Source: "previous-evidence"}
	row.Fault = "metadata read unavailable"
	row.ObservationFault = "visible passive refresh unavailable; previous evidence retained"
	entry = v.entry(row)
	wantCells := []ui.Cell{{Text: row.Handle(), Fault: true}, {Text: "claude:print"}, {Text: "unknown", Fault: true}, {Text: "undetermined", Fault: true}, {Text: "pending", Fault: true, Role: ui.RoleWarning}, {Text: "2026-01-02T03:00:00Z"}}
	if !slices.Equal(entry.Cells, wantCells) {
		t.Fatalf("retained observation was hidden or presented as fresh: %+v", entry.Cells)
	}
	fields := entryFields(entry)
	for field, want := range map[string]string{"Evidence/Status": "blocked", "Evidence/Failure code": "fixture-failure", "Evidence/Source": "previous-evidence"} {
		if fields[field] != want {
			t.Fatalf("retained evidence lost %s: %+v", field, fields)
		}
	}
	if !strings.Contains(fields["Faults/limitations/Authority"], "no action authority") {
		t.Fatalf("retained evidence lost qualification: %+v", fields)
	}
	if got := entryFaults(entry); !slices.Equal(got, []string{row.Fault, row.ObservationFault}) {
		t.Fatalf("retained entry faults lost: %+v", got)
	}
}

func entryFaults(entry ui.Entry) []string {
	var faults []string
	for _, group := range entry.Details {
		for _, field := range group.Fields {
			if field.Label == "Fault" {
				faults = append(faults, field.Value)
			}
		}
	}
	return faults
}

func TestTaskDisplayRolesKeepLifecycleDimensionsIndependent(t *testing.T) {
	for _, tc := range []struct {
		name        string
		admission   task.Admission
		liveness    task.Liveness
		publication task.Publication
		roles       [3]ui.DisplayRole
	}{
		{"unknown", task.AdmissionUnknown, task.LivenessUndetermined, task.PublicationUnknown, [3]ui.DisplayRole{ui.RoleNeutral, ui.RoleNeutral, ui.RoleNeutral}},
		{"active", task.AdmissionAdmitted, task.LivenessRunning, task.PublicationPending, [3]ui.DisplayRole{ui.RolePositive, ui.RoleWarning, ui.RoleWarning}},
		{"ended committed", task.AdmissionAdmitted, task.LivenessEnded, task.PublicationCommitted, [3]ui.DisplayRole{ui.RolePositive, ui.RoleNeutral, ui.RolePositive}},
		{"rejected", task.AdmissionNotAdmitted, task.LivenessEnded, task.PublicationRejected, [3]ui.DisplayRole{ui.RoleNegative, ui.RoleNeutral, ui.RoleNegative}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := history.Row{Identity: history.Identity{Kind: taskdir.IndexLocal, TaskID: strings.Repeat("a", 32)}, Metadata: taskdir.IndexMetadata{Provider: "committed"}, Observation: &history.Observation{Admission: tc.admission, Liveness: tc.liveness, Publication: tc.publication}}
			entry := (views{}).entry(row)
			if entry.Cells[1].Role != ui.RoleNeutral {
				t.Fatal("arbitrary provider name received lifecycle meaning")
			}
			groupIndex := slices.IndexFunc(entry.Details, func(group ui.DetailGroup) bool { return group.Title == "Evidence" })
			if groupIndex < 0 {
				t.Fatal("evidence group missing")
			}
			evidence := entry.Details[groupIndex].Fields
			if len(evidence) < 3 {
				t.Fatal("lifecycle evidence fields missing")
			}
			for i, role := range tc.roles {
				cell := entry.Cells[i+2]
				if cell.Role != role || evidence[i].Role != role || evidence[i].Value != cell.Text {
					t.Fatalf("dimension %d diverged across table and detail: cell=%+v detail=%+v want role=%d", i, cell, evidence[i], role)
				}
			}
		})
	}
}

type collectProbe struct {
	*app.Workbench
	request app.Arguments
}

func (p *collectProbe) Collect(_ context.Context, a app.Arguments) app.OperationResult {
	p.request = a
	return app.OperationResult{Code: 4, Response: app.Response{Command: "collect", TaskID: a.TaskID, Publication: "rejected", Liveness: "undetermined", Admission: "admitted"}}
}

func TestTaskCollectReportsRejectedOutcomeWithoutRetryOrFailure(t *testing.T) {
	backend := &collectProbe{}
	v := views{backend: backend, base: app.Arguments{Root: t.TempDir()}}
	id := strings.Repeat("a", 32)
	action := v.taskAction("collect", id)
	a := prepare(t, action, nil)
	feedback := action.Run(t.Context(), a)
	if backend.request.TaskID != id || feedback.Err != nil || !strings.Contains(feedback.Body, "Publication: rejected") {
		t.Fatalf("rejected outcome lost normal result semantics: %+v", feedback)
	}
}

type previewProbe struct {
	*app.Workbench
	request app.Arguments
}

func (p *previewProbe) PreviewDispatch(_ context.Context, a app.Arguments) (app.DispatchPreview, error) {
	p.request = a
	pinned := a
	pinned.TeamExpectedRevision = 7
	return app.DispatchPreview{
		Arguments: pinned, Provider: "codex:exec", Config: task.TaskConfig{Model: "member-default", Permission: "read-only", Budget: "10m"},
		Team: &task.TeamContext{Target: task.TeamTarget{Team: task.TeamReference{TeamID: "review"}, MemberID: "primary"}, TeamRevision: 7},
	}, nil
}

func TestTaskPreviewUsesOwnerResolutionAndRetainsPinnedRequest(t *testing.T) {
	backend := &previewProbe{}
	v := views{backend: backend, base: app.Arguments{Root: t.TempDir()}, cwd: t.TempDir()}
	action := v.dispatch(false)
	a := prepare(t, action, map[string]string{"Team scope": "user", "Team ID": "review", "Member ID": "primary", "Brief text": "instruction"})
	preview, err := action.Preview(t.Context(), a)
	if err != nil || preview.Arguments.TeamExpectedRevision != 7 || backend.request.TaskID != a.TaskID {
		t.Fatalf("owner preview pin was lost: %+v, %v", preview, err)
	}
	for _, wanted := range []string{"Provider: codex:exec", "Model: member-default", "Permission: read-only", "Team revision: 7"} {
		if !strings.Contains(preview.Summary, wanted) {
			t.Fatalf("resolved preview omitted %q: %s", wanted, preview.Summary)
		}
	}
	if preview.Arguments.Provider != "" {
		t.Fatal("view replaced original member-selection intent with a provider override")
	}
}

type contentProbe struct {
	*app.Workbench
	limit int64
}

func (p *contentProbe) ReadPublishedResult(_ context.Context, _ app.Arguments, limit int64) (app.ContentPreview, error) {
	p.limit = limit
	return app.ContentPreview{Data: []byte("safe\x1b]52;c;clipboard\a answer"), TotalBytes: 100000, Truncated: true}, nil
}

func (p *contentProbe) ReadLogRange(_ context.Context, a app.Arguments) (taskdir.LogRange, error) {
	p.limit = a.LogLimit
	return taskdir.LogRange{Stream: a.LogStream, Offset: a.LogOffset, Data: []byte("log\x1b[2J text"), Length: 9, ObservedSize: 9}, nil
}

func TestTaskContentViewersAreBoundedExplicitAndTerminalSafe(t *testing.T) {
	backend := &contentProbe{}
	v := views{backend: backend, base: app.Arguments{Root: t.TempDir()}}
	id := strings.Repeat("a", 32)
	result := v.result(id)
	feedback := result.Run(t.Context(), result.Arguments)
	if backend.limit != taskdir.MaxLogRangeBytes || feedback.Err != nil || strings.Contains(feedback.Body, "\x1b") || strings.Contains(feedback.Body, "clipboard") || !strings.Contains(feedback.Body, "Preview truncated") {
		t.Fatalf("unsafe or misleading result viewer: %+v", feedback)
	}
	logs := v.logRange(id)
	a := prepare(t, logs, map[string]string{"Stream": "stderr", "Limit": "4096"})
	feedback = logs.Run(t.Context(), a)
	if backend.limit != 4096 || strings.Contains(feedback.Body, "\x1b") || feedback.Err != nil || !strings.Contains(feedback.Body, "log text") {
		t.Fatalf("unsafe log viewer: %+v", feedback)
	}
	if v.logRange("remote:"+id).Kind != session.Effect {
		t.Fatal("write-capable remote log observation was treated as a passive read")
	}
}

func TestTaskSearchSelectsDeclaredHistoryOrder(t *testing.T) {
	for _, order := range []history.Order{history.OrderCreated, history.OrderObserved} {
		query, err := searchQuery("codex:exec order:" + string(order) + " publication:committed")
		if err != nil || query.Order != order || query.Search != "codex:exec" || query.Publication != "committed" {
			t.Fatalf("order selector lost: %+v %v", query, err)
		}
	}
	for _, text := range []string{"order:", "order:recent", "order:created order:observed"} {
		if _, err := searchQuery(text); err == nil {
			t.Fatalf("ambiguous history order accepted: %q", text)
		}
	}
}

func TestTaskCoverageKeepsIncompleteDiscoveryAndDatedOrderExplicit(t *testing.T) {
	strip, detail := coverage(history.Progress{Incomplete: true, Indexed: 12, Observed: 8, Unobserved: 3, Faults: 1}, history.OrderObserved)
	if strings.Contains(strip, "\n") || !strings.HasPrefix(strip, "Dated • discovery incomplete") || !strings.Contains(strip, "12 indexed • 8 observed • 3 unobserved • 1 faults") || !strings.Contains(strip, "order:observed") {
		t.Fatalf("compact coverage lost qualification or counts: %q", strip)
	}
	for _, want := range []string{"last observed", "dated evidence, not creation time", "recent among indexed records", "not current global status"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("coverage omitted %q: %s", want, detail)
		}
	}
}

func TestContinuationFormReachesCanonicalPredecessorLookup(t *testing.T) {
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	root := store.Root
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	backend := app.NewWorkbench(app.Dependencies{Environment: []string{}})
	v := views{backend: backend, base: app.Arguments{Root: root}}
	for _, preset := range []string{"", strings.Repeat("a", 32)} {
		action := v.continueTask(preset)
		request := prepare(t, action, map[string]string{"Predecessor task": strings.Repeat("b", 32)})
		result := backend.Continue(t.Context(), request)
		if request.TaskID != strings.Repeat("b", 32) || result.Code != 1 || !errors.Is(result.Err, os.ErrNotExist) {
			t.Fatalf("form failed before canonical predecessor lookup: %+v", result)
		}
	}
}
