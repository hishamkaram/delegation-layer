package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	teamcontract "github.com/hishamkaram/delegation-layer/internal/teams/contract"
)

func TestWorkbenchDispatchRejectsInvalidRequestWithoutCreatingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	w := NewWorkbench(Dependencies{Environment: []string{}})
	a := Arguments{Root: root, Provider: "codex:exec", BriefBytes: []byte("brief"), Cwd: t.TempDir()}
	a.Config.Permission = "invalid"
	result := w.Dispatch(t.Context(), a)
	// Permission validation remains at the shared request boundary.
	if result.Code != 2 || result.Err == nil || result.Response.TaskRecord == "created" {
		t.Fatalf("invalid dispatch = %+v", result)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("validation created root: %v", err)
	}
}

func TestPublishedPrefixValidatesBytesBeyondDisplayedPrefix(t *testing.T) {
	data := []byte("visible prefix and undisplayed authoritative tail")
	descriptor := task.PayloadDescriptor{Basename: "result.txt", Length: int64(len(data)), SHA256: task.ComputeSHA256(data)}
	got, err := readPublishedPrefix(t.Context(), bytes.NewReader(data), descriptor, 7)
	if err != nil || string(got.Data) != "visible" || !got.Truncated || got.TotalBytes != int64(len(data)) {
		t.Fatalf("bounded preview = %+v, %v", got, err)
	}
	data[len(data)-1] ^= 1
	got, err = readPublishedPrefix(t.Context(), bytes.NewReader(data), descriptor, 7)
	if !errors.Is(err, task.ErrEvidenceFault) || len(got.Data) != 0 {
		t.Fatalf("corrupt hidden tail exposed trusted prefix: %+v, %v", got, err)
	}
}

func TestWorkbenchPreviewPinsActualSelectionAndRejectsChangedCatalog(t *testing.T) {
	f := newTeamsScheduleFixture(t)
	a := f.arguments
	a.Config.Permission, a.Config.Budget = "read-only", "1m"
	w := NewWorkbench(f.deps)
	preview, err := w.PreviewDispatch(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Provider != "alpha:run" || preview.Config.Model != "model-one" || preview.Config.Effort != "high" || preview.Team == nil || preview.Team.TeamRevision != 1 {
		t.Fatalf("preview did not use canonical Member defaults: %+v", preview)
	}
	if preview.Arguments.Provider != a.Provider || preview.Arguments.Config.Model != a.Config.Model || preview.Arguments.ExpectedTeamSelection == nil {
		t.Fatal("preview replaced intent rather than pinning its effective selection")
	}
	preview.Team.TeamName = "display-only edit"
	if preview.Arguments.ExpectedTeamSelection.Context.TeamName == preview.Team.TeamName {
		t.Fatal("display state aliases the admission precondition")
	}
	if _, _, err := f.catalog.Update(t.Context(), f.ref, 1, teamsScheduleDefinition("alpha:run", "changed-model", "high")); err != nil {
		t.Fatal(err)
	}
	result := w.Dispatch(t.Context(), preview.Arguments)
	if !errors.Is(result.Err, teamcontract.ErrTeamSelectionChanged) {
		t.Fatalf("stale preview dispatch: %+v", result)
	}
	if len(f.requests) != 0 || result.Response.TaskRecord == "created" {
		t.Fatalf("stale preview reached provider preparation: calls=%d result=%+v", len(f.requests), result)
	}
}

func TestWorkbenchDoctorRejectsInvalidTypedRequestBeforeObservation(t *testing.T) {
	report, code := NewWorkbench(Dependencies{}).Doctor(Arguments{Provider: "codex:exec"})
	if code != 2 || len(report.Issues) != 1 || report.Issues[0].Code != "invalid_request" || report.Provider != nil {
		t.Fatalf("invalid typed doctor reached observation: code=%d report=%+v", code, report)
	}
}

func TestWorkbenchPublishedResultReadDoesNotPublishPendingSeal(t *testing.T) {
	store, td, req := newAppTestTask(t, true)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	w := NewWorkbench(Dependencies{PredicateRegistry: NativePredicates})
	if _, err := w.ReadPublishedResult(t.Context(), Arguments{Root: store.Root, TaskID: req.TaskID}, 64); err == nil {
		t.Fatal("sealed pending task was presented as published")
	}
	if _, err := os.Lstat(filepath.Join(td.Dir, "outcome.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("passive result read published or changed outcome: %v", err)
	}
	if _, cleanupErr, err := td.Collect(task.FixturePredicateRef()); err != nil || cleanupErr != nil {
		t.Fatalf("fixture collect: %v/%v", err, cleanupErr)
	}
	result, err := w.ReadPublishedResult(t.Context(), Arguments{Root: store.Root, TaskID: req.TaskID}, 1)
	if err != nil || len(result.Data) != 1 || result.TotalBytes < 1 {
		t.Fatalf("committed result = %+v, %v", result, err)
	}
	if _, err := w.ReadLogRange(t.Context(), Arguments{Root: store.Root, TaskID: req.TaskID, LogStream: "stdout", LogLimit: taskdir.MaxLogRangeBytes}); err != nil {
		t.Fatalf("local log range: %v", err)
	}
}

func TestSchedulePreparationAcceptsTypedContentWithoutTemporaryFile(t *testing.T) {
	a := scheduleCreateArgs(t)
	a.Brief = ""
	a.BriefBytes = []byte("typed schedule request")
	want := append([]byte(nil), a.BriefBytes...)
	input, err := prepareScheduleCreate(a, time.Now, func() (string, error) { return scheduleTestID, nil })
	if err != nil || !bytes.Equal(input.Brief, want) {
		t.Fatalf("typed schedule = %+v, %v", input, err)
	}
	a.BriefBytes[0] = 'X'
	if !bytes.Equal(input.Brief, want) {
		t.Fatal("schedule retained mutable input")
	}
	if _, err := os.Lstat(a.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preparation created root: %v", err)
	}
}

func TestInteractionMessageTypedInputUsesExistingDecoder(t *testing.T) {
	id := strings.Repeat("a", 32)
	message, err := interactionMessage(Arguments{Command: "steer", MessageID: id, BriefBytes: []byte("exact guidance")})
	if err != nil || message.Text != "exact guidance" || message.ID != id {
		t.Fatalf("typed steering = %+v, %v", message, err)
	}
	if _, err := interactionMessage(Arguments{Command: "reply", AnswersBytes: []byte(`{"unknown":true}`)}); err == nil {
		t.Fatal("reply bypassed the strict answer decoder")
	}
}

func TestWorkbenchScheduleReadHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	a := Arguments{Root: filepath.Join(t.TempDir(), "absent"), ScheduleAction: "list", ScheduleLimit: 50}
	result := NewWorkbench(Dependencies{}).Schedules(ctx, a)
	if result.Err == nil {
		t.Fatal("cancelled background read reported success")
	}
}

func TestWorkbenchReportsRetainedCustodyWithoutClosingIt(t *testing.T) {
	store, td, _ := newAppTestTask(t, false)
	t.Cleanup(func() { closeAppTestTask(t, store, td) })
	result := workbenchResult(commandResult{err: &taskAdmissionCleanupError{cause: errors.New("cleanup retained"), store: store}})
	if !result.RestartRequired || result.Err == nil {
		t.Fatalf("retained cleanup not surfaced: %+v", result)
	}
	if _, _, err := td.PreparedRecords(); err != nil {
		t.Fatalf("retained custody was closed: %v", err)
	}
}
