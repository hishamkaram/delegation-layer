package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/scheduler"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestScheduleCreateBusyInitializationHasNoInspectGuidance(t *testing.T) {
	a := scheduleCreateArgs(t)
	state, err := taskdir.InitStore(a.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := state.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	owner, err := scheduler.AcquireOwnerLease(t.Context(), state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := owner.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	response, code := runScheduleCreateResponse(t, a)
	assertScheduleBusyCreateResponse(t, response, code)
	assertScheduleDatabaseAbsent(t, a.Root)
	list, listCode, _ := runScheduleTest(t, a.Root, "list")
	if listCode != 0 || list.Schedules == nil || len(*list.Schedules) != 0 {
		t.Fatalf("list under owner lease code=%d response=%+v", listCode, list)
	}
	assertScheduleDatabaseAbsent(t, a.Root)
	if err = owner.Close(); err != nil {
		t.Fatal(err)
	}
	if response, code = runScheduleCreateResponse(t, a); code != 0 {
		t.Fatalf("create after owner release code=%d response=%+v", code, response)
	}
	show, showCode, _ := runScheduleTest(t, a.Root, "show", scheduleTestID)
	if showCode != 0 || show.Schedule == nil || show.Schedule.State != contract.StateEnabled {
		t.Fatalf("show after owner release code=%d response=%+v", showCode, show)
	}
}

func runScheduleCreateResponse(t *testing.T, a Arguments) (Response, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(scheduleCreateArgv(a), &stdout, &stderr, Dependencies{})
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode create response: %v; stderr=%s", err, stderr.String())
	}
	return response, code
}

func assertScheduleBusyCreateResponse(t *testing.T, response Response, code int) {
	t.Helper()
	if code != 1 || response.Schedule == nil || response.Schedule.ID != scheduleTestID || response.Error == "" {
		t.Fatalf("busy create code=%d response=%+v", code, response)
	}
	if response.Schedule.NextAction != "" {
		t.Fatalf("pre-write initialization failure requested inspection: %q", response.Schedule.NextAction)
	}
}

func assertScheduleDatabaseAbsent(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, ".scheduler", "schedules.sqlite")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scheduler database exists or cannot be inspected: %v", err)
	}
}

func TestScheduleCreateCleanupRetainsAttemptGuidanceAndFaults(t *testing.T) {
	writeFault, closeFault := errors.New("create fault"), errors.New("close fault")
	for _, tc := range []struct {
		name     string
		state    contract.State
		action   string
		cause    error
		closeErr error
		want     string
	}{
		{name: "pre-write refusal", cause: task.ErrLockBusy},
		{name: "pre-write cleanup fault", cause: task.ErrLockBusy, closeErr: closeFault},
		{name: "committed cleanup fault", state: contract.StateEnabled, closeErr: closeFault, want: "inspect_schedule"},
		{name: "uncertain attempt cleanup fault", action: "inspect_schedule", cause: writeFault, closeErr: closeFault, want: "inspect_schedule"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := newResponse("schedule")
			response.Schedule = &ScheduleResponse{ID: scheduleTestID, State: tc.state, NextAction: tc.action}
			result := commandResult{response: response}
			if tc.cause != nil {
				result = failed(response, tc.cause, 1)
			}
			mergeScheduleClose(&result, func() error { return tc.closeErr })
			if result.code != 1 || result.response.Schedule.NextAction != tc.want {
				t.Fatalf("cleanup code=%d action=%q; want 1 and %q", result.code, result.response.Schedule.NextAction, tc.want)
			}
			if tc.cause != nil && !errors.Is(result.err, tc.cause) {
				t.Fatalf("original fault lost: %v", result.err)
			}
			if tc.closeErr != nil && !errors.Is(result.err, tc.closeErr) {
				t.Fatalf("cleanup fault lost: %v", result.err)
			}
		})
	}
}
