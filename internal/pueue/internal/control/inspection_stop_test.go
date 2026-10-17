package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestStopInspectionRoutesOneTargetAndPreservesUncertainty(t *testing.T) {
	cases := []inspectionStopCase{
		{name: "queued remove", mode: "ok", state: StateQueued, action: "remove", acknowledged: boolPointer(true)},
		{name: "running kill", mode: "ok", state: StateRunning, action: "kill", acknowledged: boolPointer(true)},
		{name: "refused remove", mode: "stop-refuse", state: StateQueued, action: "remove", acknowledged: boolPointer(false)},
		{name: "ended no-op", mode: "ok", state: StateEnded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runInspectionStopCase(t, tc) })
	}
}

func TestStopQueuedInspectionRetiresTerminalTicketBeforeNumericIDReuse(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	id := int64(9)
	identity := InspectionIdentity{RootID: store.RootID, TaskID: strings.Repeat("b", 32), NumericTaskID: &id}
	writeInspectionStatus(t, fake, identity, id, StateQueued)
	if err := fake.client.RegisterPrivateTerminalEvent(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	ticketPath := filepath.Join(store.Root, privateSupervisorDirectory, privateTerminalEventTicketDir, "9.json")
	if _, err := os.Lstat(ticketPath); err != nil {
		t.Fatal(err)
	}
	permit := &inspectionTestPermit{
		stopRoot:       identity.RootID,
		stopTask:       identity.TaskID,
		stopTarget:     &id,
		stopSupervisor: fake.client.Binding(),
	}
	result, err := fake.client.StopInspection(t.Context(), permit, identity)
	if err != nil || result.Action != "remove" || result.Acknowledged == nil || !*result.Acknowledged {
		t.Fatalf("queued inspection stop result=%+v err=%v", result, err)
	}
	if _, err = os.Lstat(ticketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("queued inspection stop retained reusable callback ticket: %v", err)
	}
}

func TestStopInspectionRejectsPermitBindingsBeforeObservation(t *testing.T) {
	baseTarget := int64(7)
	identity := InspectionIdentity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32), NumericTaskID: &baseTarget}
	for _, testCase := range []struct {
		name   string
		mutate func(*inspectionTestPermit, *InspectionIdentity, task.SupervisorRef)
		want   error
	}{
		{
			name: "wrong root",
			mutate: func(permit *inspectionTestPermit, _ *InspectionIdentity, _ task.SupervisorRef) {
				permit.stopRoot = strings.Repeat("c", 32)
			},
			want: task.ErrIdentityMismatch,
		},
		{
			name: "wrong task",
			mutate: func(permit *inspectionTestPermit, _ *InspectionIdentity, _ task.SupervisorRef) {
				permit.stopTask = strings.Repeat("c", 32)
			},
			want: task.ErrIdentityMismatch,
		},
		{
			name: "wrong target",
			mutate: func(permit *inspectionTestPermit, _ *InspectionIdentity, _ task.SupervisorRef) {
				wrong := int64(8)
				permit.stopTarget = &wrong
			},
			want: task.ErrIdentityMismatch,
		},
		{
			name: "wrong supervisor",
			mutate: func(permit *inspectionTestPermit, _ *InspectionIdentity, binding task.SupervisorRef) {
				binding.ConfigDigest = task.ComputeSHA256([]byte("wrong-config"))
				permit.stopSupervisor = binding
			},
			want: ErrBinding,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newInspectionSupervisor(t, "ok")
			candidate := identity
			permitTarget := baseTarget
			permit := &inspectionTestPermit{
				stopRoot:       identity.RootID,
				stopTask:       identity.TaskID,
				stopTarget:     &permitTarget,
				stopSupervisor: fake.client.Binding(),
			}
			testCase.mutate(permit, &candidate, fake.client.Binding())
			result, err := fake.client.StopInspection(context.Background(), permit, candidate)
			if !errors.Is(err, testCase.want) {
				t.Fatalf("unsafe stop permit was accepted: result=%+v err=%v", result, err)
			}
			if permit.calls != 0 || inspectionMutationCount(t, fake) != 0 {
				t.Fatalf("stop permit mismatch consumed or observed supervisor: calls=%d invocations=%+v", permit.calls, readInvocations(t, fake.logPath))
			}
		})
	}
}

func inspectionMutationCount(t *testing.T, fake *fakeSupervisor) int {
	t.Helper()
	invocations := readInvocations(t, fake.logPath)
	return countCommand(invocations, "status") + countCommand(invocations, "group") + countCommand(invocations, "add") + countCommand(invocations, "remove") + countCommand(invocations, "kill")
}

func runInspectionStopCase(t *testing.T, tc inspectionStopCase) {
	t.Helper()
	fake, identity, id := prepareInspectionStop(t, tc)
	permit := &inspectionTestPermit{
		stopRoot:       identity.RootID,
		stopTask:       identity.TaskID,
		stopTarget:     &id,
		stopSupervisor: fake.client.Binding(),
	}
	result, err := fake.client.StopInspection(context.Background(), permit, identity)
	assertInspectionStopResult(t, tc, result, err, permit, id)
	assertInspectionStopCommand(t, fake, tc, id)
}

func prepareInspectionStop(t *testing.T, tc inspectionStopCase) (*fakeSupervisor, InspectionIdentity, int64) {
	t.Helper()
	fake := newInspectionSupervisor(t, tc.mode)
	fixture := newTaskFixture(t, fake.client.Binding())
	prepared, err := fixture.taskDir.PrepareSubmission(fake.client.Binding())
	if err != nil {
		t.Fatal(err)
	}
	record, err := prepared.Record()
	if err != nil {
		t.Fatal(err)
	}
	if releaseErr := prepared.Release(); releaseErr != nil {
		t.Fatal(releaseErr)
	}
	id := int64(7)
	identity := InspectionIdentity{RootID: record.RootID, TaskID: record.TaskID, NumericTaskID: &id}
	writeInspectionStatus(t, fake, identity, id, tc.state)
	return fake, identity, id
}

func assertInspectionStopResult(t *testing.T, tc inspectionStopCase, result StopResult, err error, permit *inspectionTestPermit, id int64) {
	t.Helper()
	assertInspectionStopError(t, tc, result, err)
	assertInspectionStopFields(t, tc, result, permit, id)
	assertInspectionStopAcknowledgment(t, tc, result)
}

func assertInspectionStopError(t *testing.T, tc inspectionStopCase, result StopResult, err error) {
	t.Helper()
	if tc.mode == "stop-refuse" {
		if err == nil || result.Acknowledged == nil || *result.Acknowledged {
			t.Fatalf("refusal did not remain uncertain: result=%+v err=%v", result, err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
}

func assertInspectionStopFields(t *testing.T, tc inspectionStopCase, result StopResult, permit *inspectionTestPermit, id int64) {
	t.Helper()
	if !result.Requested || !result.Matched || result.ObservedState != tc.state || result.NumericTaskID == nil || *result.NumericTaskID != id {
		t.Fatalf("wrong inspection stop observation: %+v", result)
	}
	if result.Action != tc.action || result.Attempted != (tc.action != "") || permit.calls != boolToInt(tc.action != "") {
		t.Fatalf("wrong inspection stop routing: result=%+v permit_calls=%d", result, permit.calls)
	}
}

func assertInspectionStopAcknowledgment(t *testing.T, tc inspectionStopCase, result StopResult) {
	t.Helper()
	if tc.acknowledged == nil {
		if result.Acknowledged != nil {
			t.Fatalf("ended inspection acquired acknowledgment: %+v", result)
		}
	} else if result.Acknowledged == nil || *result.Acknowledged != *tc.acknowledged {
		t.Fatalf("wrong inspection acknowledgment: %+v", result)
	}
}

func assertInspectionStopCommand(t *testing.T, fake *fakeSupervisor, tc inspectionStopCase, id int64) {
	t.Helper()
	invocations := readInvocations(t, fake.logPath)
	if tc.action == "" {
		if countCommand(invocations, "remove") != 0 || countCommand(invocations, "kill") != 0 {
			t.Fatalf("ended inspection mutated supervisor: %+v", invocations)
		}
		return
	}
	want := []string{"-c", fake.configPath, tc.action, strconv.FormatInt(id, 10)}
	var mutation []string
	for _, invocation := range invocations {
		if len(invocation) > 2 && invocation[2] == tc.action {
			mutation = invocation
		}
	}
	if !slices.Equal(mutation, want) {
		t.Fatalf("wrong inspection stop argv: got=%q want=%q", mutation, want)
	}
}

func TestStopInspectionWithoutSavedTargetNeverReconcilesOrMutates(t *testing.T) {
	fake := newInspectionSupervisor(t, "ok")
	identity := InspectionIdentity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32)}
	permit := &inspectionTestPermit{}
	result, err := fake.client.StopInspection(context.Background(), permit, identity)
	if !errors.Is(err, ErrUnknown) || !result.Requested || result.Attempted || result.Matched || result.NumericTaskID != nil || permit.calls != 0 {
		t.Fatalf("missing target was routed: result=%+v err=%v permit_calls=%d", result, err, permit.calls)
	}
	if countCommand(readInvocations(t, fake.logPath), "status") != 0 {
		t.Fatalf("missing target triggered status reconciliation: %+v", readInvocations(t, fake.logPath))
	}
}

func newInspectionSupervisor(t *testing.T, mode string) *fakeSupervisor {
	t.Helper()
	fake := newFakeSupervisorPaths(t, mode, "pueue "+FixtureVersion)
	if err := os.WriteFile(fake.executable, []byte(inspectionSupervisorScript), 0o700); err != nil {
		t.Fatal(err)
	}
	fake.environment = append(fake.environment, "FAKE_ADD_ID=9")
	client, err := Bind(context.Background(), fake.executable, fake.configPath, Options{ObservationTimeout: DefaultObservationTimeout, Environment: fake.environment})
	if err != nil {
		t.Fatal(err)
	}
	fake.client = client
	return fake
}

func newPrivateInspectionSupervisor(t *testing.T, mode string) (*fakeSupervisor, *taskdir.Store) {
	t.Helper()
	store, err := taskdir.InitStore(filepath.Join(canonicalTemp(t), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	fake := newFakeSupervisorPaths(t, mode, "pueue "+FixtureVersion)
	if err = os.WriteFile(fake.executable, []byte(inspectionSupervisorScript), 0o700); err != nil {
		t.Fatal(err)
	}
	fake.configPath = PrivateConfigPath(store.Root)
	if err = os.MkdirAll(filepath.Dir(fake.configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(fake.configPath, []byte(privateConfigYAML(filepath.Dir(fake.configPath))), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = WithBootstrapLock(context.Background(), store.Root, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	fake.environment = append(fake.environment, "FAKE_ADD_ID=9")
	client, err := Bind(context.Background(), fake.executable, fake.configPath, Options{ObservationTimeout: DefaultObservationTimeout, Environment: fake.environment})
	if err != nil {
		t.Fatal(err)
	}
	fake.client = client
	return fake, store
}

func writeInspectionStatus(t *testing.T, fake *fakeSupervisor, identity InspectionIdentity, id int64, state State) {
	t.Helper()
	var status map[string]any
	switch state {
	case StateStashed:
		status = map[string]any{"Stashed": map[string]any{"enqueue_at": nil}}
	case StateQueued:
		status = map[string]any{"Queued": map[string]any{"enqueued_at": statusTestTime}}
	case StateRunning:
		status = map[string]any{"Running": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime}}
	case StateEnded:
		status = map[string]any{"Done": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime, "end": statusTestTime, "result": "Success"}}
	case StateUnknown:
		t.Fatalf("unsupported inspection state %q", state)
	default:
		t.Fatalf("unsupported inspection state %q", state)
	}
	job := statusTestJob(id, identity.Label(), status)
	job["group"] = identity.Group()
	data := statusTestPayload(t, map[string]any{strconv.FormatInt(id, 10): job}, map[string]any{identity.Group(): map[string]any{"status": "Running", "parallel_tasks": 1}})
	if err := os.WriteFile(fake.statusPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
