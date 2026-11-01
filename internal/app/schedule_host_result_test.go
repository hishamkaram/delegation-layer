package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/scheduler/host"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestSchedulerHostResultCodePreservesFaults(t *testing.T) {
	fault := errors.New("host lease cleanup failed")
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{name: "completed", code: 0},
		{name: "pending", err: host.ErrPending, code: 3},
		{name: "bounded contention", err: errors.Join(host.ErrPending, task.ErrLockBusy, context.DeadlineExceeded), code: 3},
		{name: "unclassified lock failure", err: task.ErrLockBusy, code: 1},
		{name: "independent deadline", err: context.DeadlineExceeded, code: 1},
		{name: "pending with cleanup fault", err: errors.Join(host.ErrPending, task.ErrLockBusy, fault), code: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := schedulerHostResultCode(tc.err); got != tc.code {
				t.Fatalf("schedulerHostResultCode(%v)=%d, want %d", tc.err, got, tc.code)
			}
		})
	}
}

func TestSavedScheduleNeedsIndependentHost(t *testing.T) {
	response := newResponse("schedule")
	response.Schedule = &ScheduleResponse{ID: scheduleTestID}
	command := []string{"/installation with spaces/delegate", "--root", "/state with spaces", "scheduler", "serve"}
	result := savedScheduleHostResult(commandResult{response: response}, SchedulerResponse{
		State: "no_host_observed", ServeCommand: command,
	}, nil)
	if result.code != 3 || result.err != nil || !result.response.Schedule.Saved || result.response.Schedule.ID != scheduleTestID {
		t.Fatalf("saved definition was lost or reported complete: %+v", result)
	}
	if result.response.Schedule.NextAction != "start_required" || result.response.Pending == nil || result.response.Pending.Kind != "start_required" {
		t.Fatalf("missing user-start decision: %+v", result.response)
	}
	if !reflect.DeepEqual(result.response.Scheduler.ServeCommand, command) {
		t.Fatalf("command arguments changed: %q", result.response.Scheduler.ServeCommand)
	}
}

func TestSavedScheduleHostReadinessAndFaults(t *testing.T) {
	fault := errors.New("authenticated host observation failed")
	for _, tc := range []struct {
		state string
		ready bool
		fault bool
		err   error
		code  int
	}{
		{state: "ready", ready: true},
		{state: "ready", code: 3},
		{state: "backoff", code: 3},
		{state: "degraded", code: 3},
		{state: "degraded", fault: true, code: 1},
		{state: "host_unreachable", err: fault, code: 1},
	} {
		t.Run(tc.state, func(t *testing.T) {
			response := newResponse("schedule")
			response.Schedule = &ScheduleResponse{ID: scheduleTestID}
			result := savedScheduleHostResult(commandResult{response: response}, SchedulerResponse{State: tc.state, Ready: tc.ready, Fault: tc.fault}, tc.err)
			if result.code != tc.code || (!tc.fault && !errors.Is(result.err, tc.err)) || !result.response.Schedule.Saved {
				t.Fatalf("host result=%+v, expected code=%d error=%v", result, tc.code, tc.err)
			}
		})
	}
}

func TestSavedScheduleTemporaryUnreachabilityRemainsPending(t *testing.T) {
	response := newResponse("schedule")
	response.Schedule = &ScheduleResponse{ID: scheduleTestID}
	result := savedScheduleHostResult(commandResult{response: response}, SchedulerResponse{State: "degraded", LastError: "host_unreachable"}, host.ErrPending)
	if result.code != 3 || result.err != nil || result.response.Error != "" || !result.response.Schedule.Saved || result.response.Schedule.ID != scheduleTestID {
		t.Fatalf("temporary uncertainty became an operational fault or lost persistence: %+v", result)
	}
	if result.response.Pending == nil || result.response.Pending.Kind != "scheduler" || result.response.Scheduler.NextAction != "inspect_scheduler" {
		t.Fatalf("temporary uncertainty omitted observation guidance: %+v", result.response)
	}
}

func TestDefinitionFaultIsNotReplacedByHostGuidance(t *testing.T) {
	fault := errors.New("definition publication remains uncertain")
	response := newResponse("schedule")
	response.Schedule = &ScheduleResponse{ID: scheduleTestID, NextAction: "inspect_schedule"}
	result := savedScheduleHostResult(failed(response, fault, 1), SchedulerResponse{State: "no_host_observed"}, nil)
	if result.code != 1 || !errors.Is(result.err, fault) || result.response.Schedule.Saved || result.response.Scheduler != nil {
		t.Fatalf("definition fault was reclassified: %+v", result)
	}
	if result.response.Schedule.NextAction != "inspect_schedule" {
		t.Fatal("definition inspection guidance was lost")
	}
}
