package schedules

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/app"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/tui/session"
	"github.com/hishamkaram/delegation-layer/internal/tui/ui"
)

func prepareOwnerAction(t *testing.T, action ui.Action, edits map[string]string) app.Arguments {
	t.Helper()
	values := fieldDefaults(action.Fields)
	for i, field := range action.Fields {
		if value, ok := edits[field.Label]; ok {
			values[i] = value
			delete(edits, field.Label)
		}
	}
	if len(edits) > 0 {
		t.Fatalf("unknown fields: %v", edits)
	}
	a, err := action.Prepare(values)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestScheduleActionsComposeRealOwnersWithoutStartingHost(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	providerCalls, supervisorCalls := 0, 0
	backend := &scheduleOwner{Workbench: app.NewWorkbench(app.Dependencies{Environment: []string{}, PrepareCandidate: func(task.TaskRecord, []string) (commonprovider.ProfileCandidate, error) {
		providerCalls++
		return commonprovider.ProfileCandidate{}, errors.New("unexpected provider preparation")
	}, SupervisorOptions: pueue.Options{Observer: func(pueue.CommandEvent) { supervisorCalls++ }}})}
	section := NewSection(backend, app.Arguments{Root: root}, t.TempDir())
	create := section.Actions[0]
	request := prepareOwnerAction(t, create, map[string]string{"Cron": "0 */2 * * *", "Timezone": "UTC", "Auto": "true", "Brief text": "private schedule brief"})
	feedback := create.Run(t.Context(), request)
	if backend.last.Code != 3 || backend.last.Response.Schedule == nil || !backend.last.Response.Schedule.Saved {
		t.Fatalf("saved definition lost pending-host exit: %+v", backend.last)
	}
	if feedback.Err != nil || !strings.Contains(feedback.Body, "Saved: true") || !strings.Contains(feedback.Body, "start_required") {
		t.Fatalf("create did not persist through owner: %+v", feedback)
	}
	page, err := section.Load(t.Context(), ui.PageRequest{})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].ID != request.ScheduleID {
		t.Fatalf("created definition inaccessible: %+v %v", page, err)
	}
	assertScheduleHealthIsPassive(t, root, page.Entries[0])
	for _, fixture := range []struct {
		operation string
		state     contract.State
	}{
		{"pause", contract.StatePaused}, {"resume", contract.StateEnabled}, {"delete", contract.StateDeleted},
	} {
		assertScheduleControl(t, backend, page.Entries[0], fixture.operation, fixture.state)
	}
	assertScheduleHostIsAbsent(t, backend, root)
	if providerCalls != 0 || supervisorCalls != 0 {
		t.Fatalf("schedule controls invoked execution infrastructure: provider=%d supervisor=%d", providerCalls, supervisorCalls)
	}
}

func assertScheduleHostIsAbsent(t *testing.T, backend *scheduleOwner, root string) {
	t.Helper()
	status := ui.Defaults("scheduler", "status")
	status.Root = root
	observed := backend.Scheduler(t.Context(), status)
	if observed.Err != nil || observed.Response.Scheduler == nil || observed.Response.Scheduler.Ready || observed.Response.Scheduler.State != "no_host_observed" {
		t.Fatalf("schedule controls started a scheduler host: %+v", observed)
	}
}

func scheduleOwnerAction(t *testing.T, entry ui.Entry, id string) ui.Action {
	t.Helper()
	for _, action := range entry.Actions {
		if action.ID == id {
			return action
		}
	}
	t.Fatalf("missing action %s", id)
	return ui.Action{}
}

func assertScheduleHealthIsPassive(t *testing.T, root string, entry ui.Entry) {
	t.Helper()
	before := scheduleSourceSnapshot(t, root)
	health := scheduleOwnerAction(t, entry, "schedule-health")
	feedback := health.Run(t.Context(), health.Arguments)
	if feedback.Err != nil || !strings.Contains(feedback.Body, "Consecutive non-committed: 0") || !strings.Contains(feedback.Body, "Coverage complete: true") {
		t.Fatalf("health projection lost real ledger: %+v", feedback)
	}
	if !maps.Equal(before, scheduleSourceSnapshot(t, root)) {
		t.Fatal("automatic/health read changed source records")
	}
}

func scheduleSourceSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		result[path] = string(data)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

type scheduleOwner struct {
	*app.Workbench
	last app.OperationResult
}

func (w *scheduleOwner) ScheduleMutation(a app.Arguments) app.OperationResult {
	w.last = w.Workbench.ScheduleMutation(a)
	return w.last
}

func assertScheduleControl(t *testing.T, backend *scheduleOwner, entry ui.Entry, operation string, want contract.State) {
	t.Helper()
	action := scheduleOwnerAction(t, entry, "schedule-"+operation)
	if !action.Confirm || action.Kind != session.Effect || action.Arguments.ScheduleID != entry.ID {
		t.Fatalf("control lost exact confirmation: %+v", action)
	}
	if feedback := action.Run(t.Context(), action.Arguments); feedback.Err != nil {
		t.Fatalf("%s refused valid control: %+v", operation, feedback)
	}
	show := ui.Defaults("schedule", "show")
	show.Root, show.ScheduleID = action.Arguments.Root, entry.ID
	result := backend.Schedules(t.Context(), show)
	if result.Err != nil || result.Response.Schedule == nil || result.Response.Schedule.State != want {
		t.Fatalf("%s did not persist requested state %s: %+v", operation, want, result)
	}
}
