package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// A cleanup authorization is bound to the captured terminal row. Reusing its
// numeric ID, moving its label, duplicating its label or changing its state
// before removal must fail the live maintenance revalidation.
func TestPrivateCleanupRevalidatesExactTerminalCapture(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		rows   []cleanupTestRow
		remove bool
	}{
		{name: "unrelated occupant at captured ID", rows: []cleanupTestRow{{id: 0, label: "foreign", state: StateEnded}}},
		{name: "row absent"},
		{name: "duplicate exact labels", rows: []cleanupTestRow{{id: 0, label: "finished", state: StateEnded}, {id: 1, label: "finished", state: StateEnded}}},
		{name: "exact label at another ID", rows: []cleanupTestRow{{id: 0, label: "foreign", state: StateEnded}, {id: 1, label: "finished", state: StateEnded}}},
		{name: "exact row stashed", rows: []cleanupTestRow{{id: 0, label: "finished", state: StateStashed}}},
		{name: "exact row running", rows: []cleanupTestRow{{id: 0, label: "finished", state: StateRunning}}},
		{name: "unchanged exact ended row", rows: []cleanupTestRow{{id: 0, label: "finished", state: StateEnded}, {id: 1, label: "foreign", state: StateEnded}}, remove: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture, client := newMaintenanceProbeFixture(t)
			var authorizations atomic.Int32
			client.options.SuccessfulCleanupAuthorizer = func(ctx context.Context, root string, binding task.SupervisorRef, jobs []Job) (PrivateQueueCleanupPlan, error) {
				authorizations.Add(1)
				assertMaintenanceProbeAuthorization(t, fixture.stateRoot, client.Binding(), ctx.Err(), root, binding, jobs)
				writeCleanupTestRows(t, root, testCase.rows)
				return allowSuccessfulCleanupForTest(ctx, root, binding, jobs)
			}
			_, err := startPrivateMaintenancePass(t, client, func() {}).result(t)
			if testCase.remove {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrCleanupAuthorization) {
				t.Fatalf("changed capture cleanup error=%v, want authorization refusal", err)
			}
			if authorizations.Load() != 1 {
				t.Fatalf("cleanup authorization calls=%d, want one", authorizations.Load())
			}
			assertCleanupTestRemoval(t, fixture.stateRoot, testCase.remove)
		})
	}
}

type cleanupTestRow struct {
	id    int64
	label string
	state State
}

// These are authored queue responses, not copies of a program or a captured
// runtime response. The native fixture still runs the normal status command.
func writeCleanupTestRows(t *testing.T, root string, rows []cleanupTestRow) {
	t.Helper()
	// Existing wire-fixture helpers require heterogeneous JSON maps; these authored values are checked by the real decoder.
	jobs := make(map[string]any, len(rows))
	for _, row := range rows {
		var state map[string]any
		switch row.state {
		case StateEnded:
			state = statusStreamTestDoneState("Success")
		case StateRunning:
			state = map[string]any{"Running": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime}}
		case StateStashed:
			state = map[string]any{"Stashed": map[string]any{"enqueue_at": nil}}
		case StateUnknown, StateQueued:
			t.Fatalf("no cleanup fixture state for %q", row.state)
		default:
			t.Fatalf("no cleanup fixture state for %q", row.state)
		}
		jobs[strconv.FormatInt(row.id, 10)] = statusTestJob(row.id, row.label, state)
	}
	payload := statusTestPayload(t, jobs, map[string]any{"default": map[string]any{"status": "Running", "parallel_tasks": 0}})
	writePrivateSuccessorTestBytes(t, filepath.Join(root, privateSupervisorDirectory, "test-status.json"), payload)
}

func assertCleanupTestRemoval(t *testing.T, root string, removed bool) {
	t.Helper()
	path := filepath.Join(root, privateSupervisorDirectory, "test-kill-task-id")
	data, err := os.ReadFile(path)
	if !removed {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refused cleanup issued removal: %q, %v", data, err)
		}
		return
	}
	if err != nil || strings.TrimSpace(string(data)) != "0" {
		t.Fatalf("cleanup removal=%q, %v; want only ID zero", data, err)
	}
}

func TestPrivateCleanupRetainsOperationalCauses(t *testing.T) {
	fixture, client := newMaintenanceProbeFixture(t)
	releaseErr := closedBootstrapLockReleaseFault(t)
	client.options.SuccessfulCleanupAuthorizer = func(context.Context, string, task.SupervisorRef, []Job) (PrivateQueueCleanupPlan, error) {
		return PrivateQueueCleanupPlan{}, errors.Join(ErrCleanupAuthorization, releaseErr)
	}
	_, err := startPrivateMaintenancePass(t, client, func() {}).result(t)
	if !errors.Is(err, ErrCleanupAuthorization) || !errors.Is(err, releaseErr) {
		t.Fatalf("cleanup lost an operational cause: %v", err)
	}
	assertCleanupTestRemoval(t, fixture.stateRoot, false)
}
