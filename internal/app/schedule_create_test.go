package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/scheduler/contract"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func scheduleCreateArgv(a Arguments) []string {
	return []string{"--root", a.Root, "schedule", "create", "--brief", a.Brief, "--cwd", a.Cwd, "--auto", "--cron", a.ScheduleCron, "--timezone", "UTC", "--id", scheduleTestID, "--json"}
}

func TestScheduleCreatePersistsRequestedDefinition(t *testing.T) {
	a := scheduleCreateArgs(t)
	argv := append(scheduleCreateArgv(a), "--model", "model-exact", "--effort", "high", "--budget", "60m")
	var stdout, stderr bytes.Buffer
	providerCalls, supervisorCalls := 0, 0
	deps := Dependencies{PrepareCandidate: func(task.TaskRecord, []string) (commonprovider.ProfileCandidate, error) {
		providerCalls++
		return commonprovider.ProfileCandidate{}, errors.New("unexpected provider preparation")
	}}
	deps.SupervisorOptions.Observer = func(pueue.CommandEvent) { supervisorCalls++ }
	code := Run(argv, &stdout, &stderr, deps)
	var created Response
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil {
		t.Fatalf("response %q err=%v stderr=%q", stdout.String(), err, stderr.String())
	}
	assertScheduleStartRequired(t, code, created)
	if created.Schedule.ID != scheduleTestID || created.Schedule.State != contract.StateEnabled || providerCalls != 0 || supervisorCalls != 0 || len(created.ScheduleWarnings) == 0 || strings.Contains(stdout.String(), "requested brief") {
		t.Fatalf("create code=%d response=%+v provider calls=%d supervisor calls=%d", code, created, providerCalls, supervisorCalls)
	}
	assertCreatedScheduleShow(t, a.Root)
	stdout.Reset()
	stderr.Reset()
	code = Run(argv, &stdout, &stderr, deps)
	var conflict Response
	if err := json.Unmarshal(stdout.Bytes(), &conflict); err != nil || code != 1 || conflict.Schedule == nil || conflict.Schedule.ID != scheduleTestID || conflict.Schedule.NextAction != "inspect_schedule" {
		t.Fatalf("duplicate code=%d response=%+v err=%v", code, conflict, err)
	}
}

func assertScheduleStartRequired(t *testing.T, code int, response Response) {
	t.Helper()
	if code != 3 || response.Schedule == nil || !response.Schedule.Saved || response.Schedule.NextAction != "start_required" {
		t.Fatalf("definition was not reported saved with start_required: code=%d response=%+v", code, response)
	}
	if response.Scheduler == nil || response.Scheduler.NextAction != "start_required" || response.Scheduler.State != "no_host_observed" || response.Scheduler.Ready {
		t.Fatalf("saved definition reported an incorrect host observation: %+v", response.Scheduler)
	}
	if response.Pending == nil || response.Pending.Kind != "start_required" || len(response.Scheduler.ServeCommand) == 0 {
		t.Fatalf("saved definition omitted its start guidance: %+v", response)
	}
}

func assertCreatedScheduleShow(t *testing.T, root string) {
	t.Helper()
	show, code, output := runScheduleTest(t, root, "show", scheduleTestID)
	if code != 0 || show.Schedule == nil || show.Schedule.Request == nil || show.Schedule.Request.Config.Model != "model-exact" || show.Schedule.Request.Config.Budget != "60m" || show.Schedule.Request.Config.Permission != "" || show.Schedule.Timezone != "UTC" || strings.Contains(output, "requested brief") {
		t.Fatalf("show code=%d response=%+v", code, show)
	}
}

func TestScheduleCreateRejectsInputsWithoutInitializing(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func([]string) []string
	}{
		{"empty timezone", func(argv []string) []string { return replaceScheduleArg(argv, "--timezone", "") }},
		{"local timezone", func(argv []string) []string { return replaceScheduleArg(argv, "--timezone", "Local") }},
		{"invalid cron", func(argv []string) []string { return replaceScheduleArg(argv, "--cron", "@daily") }},
		{"invalid model", func(argv []string) []string { return append(argv, "--model", " padded ") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := scheduleCreateArgs(t)
			var stdout, stderr bytes.Buffer
			code := Run(tc.edit(scheduleCreateArgv(a)), &stdout, &stderr, Dependencies{})
			var response Response
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || code != 2 || response.Error == "" {
				t.Fatalf("code=%d output=%q stderr=%q err=%v", code, stdout.String(), stderr.String(), err)
			}
			if _, err := os.Lstat(a.Root); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid request initialized root: %v", err)
			}
		})
	}
}

func TestScheduleCreateOmittedTimezoneSnapshotsNativeDefault(t *testing.T) {
	t.Setenv("TZ", "")
	a := scheduleCreateArgs(t)
	argv := []string{"--root", a.Root, "schedule", "create", "--brief", a.Brief, "--cwd", a.Cwd, "--auto", "--cron", a.ScheduleCron, "--id", scheduleTestID, "--json"}
	var stdout, stderr bytes.Buffer
	if code := Run(argv, &stdout, &stderr, Dependencies{}); code != 3 {
		t.Fatalf("create code=%d output=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	t.Setenv("TZ", "Europe/Berlin")
	show, code, _ := runScheduleTest(t, a.Root, "show", scheduleTestID)
	if code != 0 || show.Schedule == nil || show.Schedule.Timezone != "UTC" {
		t.Fatalf("show code=%d response=%+v", code, show)
	}
}

func TestScheduleCreateMaximumBriefIsNotEmitted(t *testing.T) {
	a := scheduleCreateArgs(t)
	brief := bytes.Repeat([]byte("b"), task.MaxBriefSize)
	if err := os.WriteFile(a.Brief, brief, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run(scheduleCreateArgv(a), &stdout, &stderr, Dependencies{}); code != 3 {
		t.Fatalf("create code=%d output=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	show, code, output := runScheduleTest(t, a.Root, "show", scheduleTestID)
	if code != 0 || show.Schedule == nil || show.Schedule.BriefLength != task.MaxBriefSize || len(output) > 4096 || strings.Contains(output, string(brief[:64])) {
		t.Fatalf("show code=%d output bytes=%d response=%+v", code, len(output), show)
	}
}

func replaceScheduleArg(argv []string, name, value string) []string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == name {
			argv[i+1] = value
			return argv
		}
	}
	return argv
}

type scheduleCreateFaultStore struct {
	contract.Store
	writeErr    error
	readErr     error
	beforeWrite bool
	writes      int
	reads       int
}

func (s *scheduleCreateFaultStore) CreateSchedule(ctx context.Context, definition contract.Definition) error {
	s.writes++
	if s.beforeWrite {
		return s.writeErr
	}
	if err := s.Store.CreateSchedule(ctx, definition); err != nil {
		return err
	}
	return s.writeErr
}

func (s *scheduleCreateFaultStore) GetSchedule(ctx context.Context, id string) (contract.Schedule, error) {
	s.reads++
	if s.readErr != nil {
		return contract.Schedule{}, s.readErr
	}
	return s.Store.GetSchedule(ctx, id)
}

func TestScheduleCreateUncertainWriteReadsOnceAndRetainsFaults(t *testing.T) {
	_, store, definition := scheduleFixture(t)
	definition.ScheduleID = strings.Repeat("d", 32)
	writeFault := errors.New("commit cleanup fault")
	wrapped := &scheduleCreateFaultStore{Store: store, writeErr: writeFault}
	response := newResponse("schedule")
	response.Schedule = &ScheduleResponse{ID: definition.ScheduleID, State: contract.StateEnabled}
	result := createSchedule(t.Context(), wrapped, definition, response)
	if result.code != 1 || !errors.Is(result.err, writeFault) || wrapped.writes != 1 || wrapped.reads != 1 || !result.response.Schedule.DefinitionMatched || result.response.Schedule.NextAction != "inspect_schedule" {
		t.Fatalf("result=%+v writes=%d reads=%d", result, wrapped.writes, wrapped.reads)
	}
	definition.ScheduleID = strings.Repeat("e", 32)
	readFault := errors.New("readback storage fault")
	wrapped = &scheduleCreateFaultStore{Store: store, writeErr: writeFault, readErr: readFault}
	response.Schedule = &ScheduleResponse{ID: definition.ScheduleID, State: contract.StateEnabled}
	result = createSchedule(t.Context(), wrapped, definition, response)
	if result.code != 1 || !errors.Is(result.err, writeFault) || !errors.Is(result.err, readFault) || wrapped.writes != 1 || wrapped.reads != 1 || result.response.Schedule.DefinitionMatched {
		t.Fatalf("result=%+v writes=%d reads=%d", result, wrapped.writes, wrapped.reads)
	}
}

func TestScheduleCreateUncertainWriteReadsAbsenceOnce(t *testing.T) {
	_, store, definition := scheduleFixture(t)
	definition.ScheduleID = strings.Repeat("f", 32)
	writeFault := errors.New("pre-write storage fault")
	wrapped := &scheduleCreateFaultStore{Store: store, writeErr: writeFault, beforeWrite: true}
	response := newResponse("schedule")
	response.Schedule = &ScheduleResponse{ID: definition.ScheduleID, State: contract.StateEnabled}
	result := createSchedule(t.Context(), wrapped, definition, response)
	if result.code != 1 || !errors.Is(result.err, writeFault) || !errors.Is(result.err, os.ErrNotExist) || wrapped.writes != 1 || wrapped.reads != 1 || result.response.Schedule.ID != definition.ScheduleID || result.response.Schedule.DefinitionMatched || result.response.Schedule.NextAction != "inspect_schedule" {
		t.Fatalf("result=%+v writes=%d reads=%d", result, wrapped.writes, wrapped.reads)
	}
	if _, err := store.GetSchedule(t.Context(), definition.ScheduleID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent definition was created: %v", err)
	}
}

func TestScheduleCreateRangeExhaustionDoesNotInitialize(t *testing.T) {
	a := scheduleCreateArgs(t)
	a.ScheduleCron = "0 0 31 2 *"
	var stdout, stderr bytes.Buffer
	code := Run(scheduleCreateArgv(a), &stdout, &stderr, Dependencies{})
	var response Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || code != 1 || response.Error == "" {
		t.Fatalf("code=%d output=%q stderr=%q err=%v", code, stdout.String(), stderr.String(), err)
	}
	if _, err := os.Lstat(a.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exhausted recurrence initialized root: %v", err)
	}
}
