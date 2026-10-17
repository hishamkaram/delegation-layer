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
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func prepareStagedOrdinarySubmission(t *testing.T, fake *fakeSupervisor) (*taskFixture, *taskdir.SubmissionPermit, Identity, Launch) {
	t.Helper()
	fixture := newTaskFixture(t, fake.client.Binding())
	permit, err := fixture.taskDir.PrepareSubmission(fake.client.Binding())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		releaseStagedSubmissionPermit(t, permit)
	})
	record, err := permit.Record()
	if err != nil {
		t.Fatal(err)
	}
	return fixture, permit, identityFromRecord(record), Launch{
		RunnerExecutable: fake.executable,
		RootPath:         fixture.store.Root,
		Supervised:       true,
	}
}

func releaseStagedSubmissionPermit(t *testing.T, permit *taskdir.SubmissionPermit) {
	t.Helper()
	releaseErr := permit.Release()
	if releaseErr != nil {
		t.Error(releaseErr)
	}
}

func appendStagedEnvironment(fake *fakeSupervisor, values ...string) {
	fake.client.options.Environment = append(append([]string(nil), fake.client.options.Environment...), values...)
}

func stagedInvocations(t *testing.T, fake *fakeSupervisor, command string) [][]string {
	t.Helper()
	var result [][]string
	for _, invocation := range readInvocations(t, fake.logPath) {
		if len(invocation) <= 2 || invocation[2] != command {
			continue
		}
		if len(invocation) > 3 && invocation[3] == "--help" {
			continue
		}
		result = append(result, invocation)
	}
	return result
}

func stagedHelpInvocations(t *testing.T, fake *fakeSupervisor, command string) [][]string {
	t.Helper()
	var result [][]string
	for _, invocation := range readInvocations(t, fake.logPath) {
		if len(invocation) > 3 && invocation[2] == command && invocation[3] == "--help" {
			result = append(result, invocation)
		}
	}
	return result
}

func stagedStateForTest(t *testing.T, state State) map[string]any {
	t.Helper()
	switch state {
	case StateStashed:
		return map[string]any{"Stashed": map[string]any{"enqueue_at": nil, "future_stashed_field": true}}
	case StateQueued:
		return map[string]any{"Queued": map[string]any{"enqueued_at": statusTestTime}}
	case StateRunning:
		return map[string]any{"Running": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime}}
	case StateEnded:
		return map[string]any{"Done": map[string]any{"enqueued_at": statusTestTime, "start": statusTestTime, "end": statusTestTime, "result": "Success"}}
	case StateUnknown:
		return map[string]any{"Locked": map[string]any{"previous_status": map[string]any{"Queued": map[string]any{"enqueued_at": statusTestTime}}}}
	default:
		t.Fatalf("unsupported staged test state %q", state)
		return nil
	}
}

func stagedRecoveryStateForTest(t *testing.T, state State, scheduled bool) map[string]any {
	t.Helper()
	if scheduled {
		return map[string]any{"Stashed": map[string]any{"enqueue_at": statusTestTime}}
	}
	return stagedStateForTest(t, state)
}

func writeStagedOrdinaryStatus(t *testing.T, fake *fakeSupervisor, identity Identity, id int64, state State, label, group string) {
	t.Helper()
	writeStagedOrdinaryStatusState(t, fake, identity, id, stagedStateForTest(t, state), label, group)
}

func writeStagedOrdinaryStatusState(t *testing.T, fake *fakeSupervisor, identity Identity, id int64, state map[string]any, label, group string) {
	t.Helper()
	if label == "" {
		label = identity.Label()
	}
	job := statusTestJob(id, label, state)
	job["group"] = group
	data := statusTestPayload(t, map[string]any{strconv.FormatInt(id, 10): job}, map[string]any{
		group: map[string]any{"status": "Running", "parallel_tasks": 0},
	})
	if err := os.WriteFile(fake.statusPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeStagedInspectionStatus(t *testing.T, fake *fakeSupervisor, identity InspectionIdentity, id int64, state State, label, group string) {
	t.Helper()
	writeStagedInspectionStatusState(t, fake, identity, id, stagedStateForTest(t, state), label, group)
}

func writeStagedInspectionStatusWithLaunch(t *testing.T, fake *fakeSupervisor, identity InspectionIdentity, id int64, state State, launch Launch) {
	t.Helper()
	writeStagedInspectionStatusStateWithCommand(t, fake, identity, id, stagedStateForTest(t, state), "", identity.Group(), stagedInspectionCommand(launch, identity))
}

func writeStagedInspectionStatusState(t *testing.T, fake *fakeSupervisor, identity InspectionIdentity, id int64, state map[string]any, label, group string) {
	t.Helper()
	writeStagedInspectionStatusStateWithCommand(t, fake, identity, id, state, label, group, "")
}

func writeStagedInspectionStatusStateWithCommand(t *testing.T, fake *fakeSupervisor, identity InspectionIdentity, id int64, state map[string]any, label, group, command string) {
	t.Helper()
	if label == "" {
		label = identity.Label()
	}
	if group == "" {
		group = identity.Group()
	}
	job := statusTestJob(id, label, state)
	job["group"] = group
	if command != "" {
		job["original_command"] = command
		job["command"] = command
	}
	parallel := 1
	if identity.Group() == "default" {
		parallel = 0
	}
	data := statusTestPayload(t, map[string]any{strconv.FormatInt(id, 10): job}, map[string]any{
		identity.Group(): map[string]any{"status": "Running", "parallel_tasks": parallel},
	})
	if err := os.WriteFile(fake.statusPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertExactOrdinaryStagedObservation(t *testing.T, candidate Observation, identity Identity) {
	t.Helper()
	if candidate.State != StateStashed || !candidate.Matched || candidate.Group != "default" || candidate.Identity.Label() != identity.Label() {
		t.Fatalf("commit did not receive the exact Stashed receipt: %+v", candidate)
	}
}

func captureOrdinaryStagedCommit(t *testing.T, fake *fakeSupervisor, identity Identity, committed *Observation) SubmissionCommit {
	t.Helper()
	return func(candidate Observation) error {
		*committed = candidate
		assertExactOrdinaryStagedObservation(t, candidate, identity)
		if enqueueCount := len(stagedInvocations(t, fake, "enqueue")); enqueueCount != 0 {
			t.Fatalf("enqueue ran before commit: %d invocations", enqueueCount)
		}
		return nil
	}
}

func assertInitialOrdinaryStagedCommands(t *testing.T, fake *fakeSupervisor) {
	t.Helper()
	adds := stagedInvocations(t, fake, "add")
	if len(adds) != 1 || !slices.Contains(adds[0], "--stashed") || slices.Contains(adds[0], "--immediate") {
		t.Fatalf("staged add command was not issued exactly once: %+v", adds)
	}
	enqueues := stagedInvocations(t, fake, "enqueue")
	if len(enqueues) != 1 || !slices.Equal(enqueues[0], []string{"-c", fake.configPath, "enqueue", "7"}) {
		t.Fatalf("wrong exact enqueue command: %+v", enqueues)
	}
}

func installFailFirstEnqueueObserver(t *testing.T, fake *fakeSupervisor) *failFirstEnqueueObserver {
	t.Helper()
	observer := &failFirstEnqueueObserver{fake: fake, hidden: fake.executable + ".enqueue-hidden", failNext: true}
	fake.client.options.Observer = observer.observe
	t.Cleanup(func() {
		observer.restore(t)
	})
	return observer
}

func (o *failFirstEnqueueObserver) observe(event CommandEvent) {
	if !slices.Contains(event.Argv, "enqueue") || slices.Contains(event.Argv, "--help") {
		return
	}
	switch event.Stage {
	case "entry":
		o.enqueueEntries++
		if o.failNext {
			o.failNext = false
			o.observerErr = os.Rename(o.fake.executable, o.hidden)
			o.moved = o.observerErr == nil
		}
	case "completed":
		if event.PID == 0 && o.moved {
			restoreErr := os.Rename(o.hidden, o.fake.executable)
			o.observerErr = errors.Join(o.observerErr, restoreErr)
			o.moved = restoreErr != nil
		}
	}
}

func (o *failFirstEnqueueObserver) restore(t *testing.T) {
	t.Helper()
	if !o.moved {
		return
	}
	restoreErr := os.Rename(o.hidden, o.fake.executable)
	if restoreErr != nil {
		t.Error(restoreErr)
		return
	}
	o.moved = false
}

func installEnqueueStatusTransition(fake *fakeSupervisor, status []byte) *enqueueStatusTransition {
	transition := &enqueueStatusTransition{fake: fake, status: status, entered: make(chan struct{})}
	fake.client.options.Observer = transition.observe
	return transition
}

func (t *enqueueStatusTransition) observe(event CommandEvent) {
	if event.Stage != "entry" || !slices.Contains(event.Argv, "enqueue") || slices.Contains(event.Argv, "--help") {
		return
	}
	t.entries.Add(1)
	t.once.Do(func() {
		t.writeErr = os.WriteFile(t.fake.statusPath, t.status, 0o600)
		close(t.entered)
	})
}

func (t *enqueueStatusTransition) wait(testingT *testing.T) {
	testingT.Helper()
	select {
	case <-t.entered:
	case <-time.After(2 * time.Second):
		testingT.Fatal("first recovery did not reach enqueue")
	}
}

func ordinaryQueuedStatus(t *testing.T, identity Identity, id int64) []byte {
	t.Helper()
	job := statusTestJob(id, identity.Label(), stagedStateForTest(t, StateQueued))
	job["group"] = "default"
	return statusTestPayload(t, map[string]any{strconv.FormatInt(id, 10): job}, map[string]any{
		"default": map[string]any{"status": "Running", "parallel_tasks": 0},
	})
}

func inspectionQueuedStatus(t *testing.T, identity InspectionIdentity, id int64) []byte {
	t.Helper()
	job := statusTestJob(id, identity.Label(), stagedStateForTest(t, StateQueued))
	job["group"] = identity.Group()
	return statusTestPayload(t, map[string]any{strconv.FormatInt(id, 10): job}, map[string]any{
		identity.Group(): map[string]any{"status": "Running", "parallel_tasks": 1},
	})
}

func inspectionQueuedStatusWithLaunch(t *testing.T, identity InspectionIdentity, id int64, launch Launch) []byte {
	t.Helper()
	job := statusTestJob(id, identity.Label(), stagedStateForTest(t, StateQueued))
	job["group"] = identity.Group()
	command := stagedInspectionCommand(launch, identity)
	job["original_command"] = command
	job["command"] = command
	parallel := uint64(1)
	if identity.Group() == "default" {
		parallel = 0
	}
	return statusTestPayload(t, map[string]any{strconv.FormatInt(id, 10): job}, map[string]any{
		identity.Group(): map[string]any{"status": "Running", "parallel_tasks": parallel},
	})
}

func startOrdinaryRecovery(client *Client, identity Identity, id int64) <-chan ordinaryRecoveryResult {
	done := make(chan ordinaryRecoveryResult, 1)
	go func() {
		observation, recoveryErr := client.RecoverStaged(context.Background(), identity, id, func(candidate Observation) error {
			if candidate.State != StateStashed {
				return errors.New("first recovery did not commit Stashed state")
			}
			return nil
		})
		done <- ordinaryRecoveryResult{observation: observation, err: recoveryErr}
	}()
	return done
}

func startInspectionRecovery(client *Client, identity InspectionIdentity, id int64) <-chan inspectionRecoveryResult {
	done := make(chan inspectionRecoveryResult, 1)
	go func() {
		observation, recoveryErr := client.RecoverInspectionStaged(context.Background(), identity, id, func(candidate InspectionObservation) error {
			if candidate.Job.State != StateStashed {
				return errors.New("first inspection recovery did not commit Stashed state")
			}
			return nil
		})
		done <- inspectionRecoveryResult{observation: observation, err: recoveryErr}
	}()
	return done
}

func runOrdinaryStagedRecoveryCase(t *testing.T, testCase ordinaryStagedRecoveryCase) {
	t.Helper()
	fake := newFakeSupervisor(t, "ok")
	_, _, identity, _ := prepareStagedOrdinarySubmission(t, fake)
	state := stagedRecoveryStateForTest(t, testCase.state, testCase.scheduled)
	writeStagedOrdinaryStatusState(t, fake, identity, testCase.rowID, state, testCase.label, "default")
	commits := 0
	observation, recoveryErr := fake.client.RecoverStaged(context.Background(), identity, 7, func(candidate Observation) error {
		commits++
		if candidate.State != StateStashed || candidate.NumericTaskID != 7 {
			t.Fatalf("recovery callback received the wrong row: %+v", candidate)
		}
		return nil
	})
	enqueueCount := len(stagedInvocations(t, fake, "enqueue"))
	assertOrdinaryStagedRecovery(t, testCase.expectation, observation, recoveryErr, commits, enqueueCount)
}

func assertOrdinaryStagedRecovery(t *testing.T, expectation stagedRecoveryExpectation, observation Observation, recoveryErr error, commits, enqueueCount int) {
	t.Helper()
	switch expectation {
	case expectStashedRecovery:
		assertSuccessfulOrdinaryStashedRecovery(t, observation, recoveryErr, commits, enqueueCount)
	case expectQueuedRejection:
		assertRejectedOrdinaryQueuedRecovery(t, observation, recoveryErr, commits, enqueueCount)
	case expectRejectedRecovery:
		assertRejectedOrdinaryRecovery(t, observation, recoveryErr, commits, enqueueCount)
	default:
		t.Fatalf("unknown ordinary recovery expectation %d", expectation)
	}
}

func assertSuccessfulOrdinaryStashedRecovery(t *testing.T, observation Observation, recoveryErr error, commits, enqueueCount int) {
	t.Helper()
	if recoveryErr != nil || commits != 1 || !observation.Matched || observation.State != StateStashed || enqueueCount != 1 {
		t.Fatalf("exact Stashed recovery failed: observation=%+v err=%v commits=%d enqueues=%d", observation, recoveryErr, commits, enqueueCount)
	}
}

func assertRejectedOrdinaryQueuedRecovery(t *testing.T, observation Observation, recoveryErr error, commits, enqueueCount int) {
	t.Helper()
	if !errors.Is(recoveryErr, ErrUnknown) || commits != 0 || !observation.Matched || observation.State != StateQueued || enqueueCount != 0 {
		t.Fatalf("already-queued recovery was not rejected: observation=%+v err=%v commits=%d enqueues=%d", observation, recoveryErr, commits, enqueueCount)
	}
}

func assertRejectedOrdinaryRecovery(t *testing.T, observation Observation, recoveryErr error, commits, enqueueCount int) {
	t.Helper()
	if !errors.Is(recoveryErr, ErrUnknown) || commits != 0 || enqueueCount != 0 {
		t.Fatalf("unsafe recovery was accepted: observation=%+v err=%v commits=%d enqueues=%d", observation, recoveryErr, commits, enqueueCount)
	}
}

func runInspectionStagedRecoveryCase(t *testing.T, testCase inspectionStagedRecoveryCase) {
	t.Helper()
	fake := newInspectionSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	identity := InspectionIdentity{RootID: fixture.store.RootID, TaskID: fixture.taskDir.TaskID}
	group := testCase.group
	if group == "" {
		group = identity.Group()
	}
	state := stagedRecoveryStateForTest(t, testCase.state, testCase.scheduled)
	writeStagedInspectionStatusState(t, fake, identity, testCase.rowID, state, testCase.label, group)
	commits := 0
	observation, recoveryErr := fake.client.RecoverInspectionStaged(context.Background(), identity, 9, func(candidate InspectionObservation) error {
		commits++
		if candidate.Job.State != StateStashed || candidate.Job.ID != 9 {
			t.Fatalf("inspection recovery callback received the wrong row: %+v", candidate)
		}
		return nil
	})
	enqueueCount := len(stagedInvocations(t, fake, "enqueue"))
	assertInspectionStagedRecovery(t, testCase.expectation, observation, recoveryErr, commits, enqueueCount)
}

func assertInspectionStagedRecovery(t *testing.T, expectation stagedRecoveryExpectation, observation InspectionObservation, recoveryErr error, commits, enqueueCount int) {
	t.Helper()
	switch expectation {
	case expectStashedRecovery:
		assertSuccessfulInspectionStashedRecovery(t, observation, recoveryErr, commits, enqueueCount)
	case expectQueuedRejection:
		assertRejectedInspectionQueuedRecovery(t, observation, recoveryErr, commits, enqueueCount)
	case expectRejectedRecovery:
		assertRejectedInspectionRecovery(t, observation, recoveryErr, commits, enqueueCount)
	default:
		t.Fatalf("unknown inspection recovery expectation %d", expectation)
	}
}

func assertSuccessfulInspectionStashedRecovery(t *testing.T, observation InspectionObservation, recoveryErr error, commits, enqueueCount int) {
	t.Helper()
	if recoveryErr != nil || commits != 1 || !observation.Matched || observation.Job.State != StateStashed || enqueueCount != 1 {
		t.Fatalf("exact Stashed inspection recovery failed: observation=%+v err=%v commits=%d enqueues=%d", observation, recoveryErr, commits, enqueueCount)
	}
}

func assertRejectedInspectionQueuedRecovery(t *testing.T, observation InspectionObservation, recoveryErr error, commits, enqueueCount int) {
	t.Helper()
	if !errors.Is(recoveryErr, ErrUnknown) || commits != 0 || !observation.Matched || observation.Job.State != StateQueued || enqueueCount != 0 {
		t.Fatalf("already-queued inspection recovery was not rejected: observation=%+v err=%v commits=%d enqueues=%d", observation, recoveryErr, commits, enqueueCount)
	}
}

func assertRejectedInspectionRecovery(t *testing.T, observation InspectionObservation, recoveryErr error, commits, enqueueCount int) {
	t.Helper()
	if !errors.Is(recoveryErr, ErrUnknown) || commits != 0 || enqueueCount != 0 {
		t.Fatalf("unsafe inspection recovery was accepted: observation=%+v err=%v commits=%d enqueues=%d", observation, recoveryErr, commits, enqueueCount)
	}
}

func captureInspectionStagedCommit(t *testing.T, fake *fakeSupervisor, identity InspectionIdentity, committed *InspectionObservation) InspectionSubmissionCommit {
	t.Helper()
	return func(candidate InspectionObservation) error {
		*committed = candidate
		assertExactInspectionStagedObservation(t, candidate, identity)
		if !candidate.TerminalEventRegistrationKnown || candidate.TerminalEventRegistered {
			t.Fatalf("external inspection registration state was not preserved: %+v", candidate)
		}
		if enqueueCount := len(stagedInvocations(t, fake, "enqueue")); enqueueCount != 0 {
			t.Fatalf("inspection enqueue ran before commit: %d", enqueueCount)
		}
		return nil
	}
}

func assertExactInspectionStagedObservation(t *testing.T, candidate InspectionObservation, identity InspectionIdentity) {
	t.Helper()
	if !candidate.Matched || candidate.Job.State != StateStashed || candidate.Job.ID != 9 || candidate.Job.Group != identity.Group() || candidate.Job.Label == nil || *candidate.Job.Label != identity.Label() {
		t.Fatalf("inspection commit did not receive exact Stashed receipt: %+v", candidate)
	}
}

func TestStagedLaunchCapabilityUsesObservedHelpSurface(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	capability, err := fake.client.StagedLaunchCapability(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !capability.AddStashed || !capability.EnqueueByID {
		t.Fatalf("staged capability was not observed: %+v", capability)
	}
	if len(stagedHelpInvocations(t, fake, "add")) != 1 || len(stagedHelpInvocations(t, fake, "enqueue")) != 1 {
		t.Fatalf("capability did not probe both command surfaces: %+v", readInvocations(t, fake.logPath))
	}
}

func TestSubmitStagedUsesRecordedGenerationCapabilityWithoutHelpProbe(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	_, permit, identity, launch := prepareStagedOrdinarySubmission(t, fake)
	if err := os.WriteFile(fake.logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	capability := StagedLaunchCapability{AddStashed: true, EnqueueByID: true}
	gate := func(context.Context, task.SupervisorRef) (SubmissionAuthorization, error) {
		return SubmissionAuthorization{StagedLaunchCapability: &capability}, nil
	}
	var committed Observation
	observation, err := fake.client.SubmitStagedWithGate(context.Background(), permit, launch, gate, captureOrdinaryStagedCommit(t, fake, identity, &committed))
	if err != nil || observation != committed || !observation.Matched {
		t.Fatalf("authorized staged submission failed: observation=%+v committed=%+v err=%v", observation, committed, err)
	}
	if addHelp, enqueueHelp := len(stagedHelpInvocations(t, fake, "add")), len(stagedHelpInvocations(t, fake, "enqueue")); addHelp != 0 || enqueueHelp != 0 {
		t.Fatalf("recorded capability repeated help probes: add=%d enqueue=%d invocations=%+v", addHelp, enqueueHelp, readInvocations(t, fake.logPath))
	}
}

func TestSubmitConcurrentInspectionStagedUsesRecordedGenerationCapabilityWithoutHelpProbe(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	identity := InspectionIdentity{RootID: store.RootID, TaskID: strings.Repeat("b", 32), QueueGroup: "default"}
	launch := Launch{RunnerExecutable: fake.executable, RootPath: store.Root, Supervised: true}
	writeStagedInspectionStatusWithLaunch(t, fake, identity, 9, StateStashed, launch)
	permit := &concurrentInspectionTestPermit{
		inspectionTestPermit: &inspectionTestPermit{
			submissionRoot:       identity.RootID,
			submissionTask:       identity.TaskID,
			submissionSupervisor: fake.client.Binding(),
		},
		queueGroup: "default",
	}
	if err := os.WriteFile(fake.logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	capability := StagedLaunchCapability{AddStashed: true, EnqueueByID: true}
	gate := func(context.Context, task.SupervisorRef) (SubmissionAuthorization, error) {
		return SubmissionAuthorization{StagedLaunchCapability: &capability}, nil
	}
	observation, err := fake.client.SubmitConcurrentInspectionStagedWithGate(context.Background(), permit, identity, launch, gate, func(InspectionObservation) error { return nil })
	if err != nil || !observation.Matched {
		t.Fatalf("authorized staged inspection failed: observation=%+v err=%v", observation, err)
	}
	if addHelp, enqueueHelp := len(stagedHelpInvocations(t, fake, "add")), len(stagedHelpInvocations(t, fake, "enqueue")); addHelp != 0 || enqueueHelp != 0 {
		t.Fatalf("recorded inspection capability repeated help probes: add=%d enqueue=%d invocations=%+v", addHelp, enqueueHelp, readInvocations(t, fake.logPath))
	}
}

func TestRecoverStagedUsesRecordedGenerationCapabilityWithoutHelpProbe(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	_, _, identity, _ := prepareStagedOrdinarySubmission(t, fake)
	writeStagedOrdinaryStatus(t, fake, identity, 7, StateStashed, "", "default")
	if err := os.WriteFile(fake.logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	capability := StagedLaunchCapability{AddStashed: true, EnqueueByID: true}
	gate := func(context.Context, task.SupervisorRef) (SubmissionAuthorization, error) {
		return SubmissionAuthorization{StagedLaunchCapability: &capability}, nil
	}
	observation, err := fake.client.RecoverStagedWithGate(context.Background(), identity, 7, gate, func(Observation) error { return nil })
	if err != nil || !observation.Matched || observation.State != StateStashed {
		t.Fatalf("authorized staged recovery failed: observation=%+v err=%v", observation, err)
	}
	if addHelp, enqueueHelp := len(stagedHelpInvocations(t, fake, "add")), len(stagedHelpInvocations(t, fake, "enqueue")); addHelp != 0 || enqueueHelp != 0 {
		t.Fatalf("recorded recovery capability repeated help probes: add=%d enqueue=%d invocations=%+v", addHelp, enqueueHelp, readInvocations(t, fake.logPath))
	}
}

func TestRecoverInspectionStagedUsesRecordedGenerationCapabilityWithoutHelpProbe(t *testing.T) {
	fake := newInspectionSupervisor(t, "ok")
	fixture := newTaskFixture(t, fake.client.Binding())
	identity := InspectionIdentity{RootID: fixture.store.RootID, TaskID: fixture.taskDir.TaskID}
	writeStagedInspectionStatus(t, fake, identity, 9, StateStashed, "", identity.Group())
	if err := os.WriteFile(fake.logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	capability := StagedLaunchCapability{AddStashed: true, EnqueueByID: true}
	gate := func(context.Context, task.SupervisorRef) (SubmissionAuthorization, error) {
		return SubmissionAuthorization{StagedLaunchCapability: &capability}, nil
	}
	observation, err := fake.client.RecoverInspectionStagedWithGate(context.Background(), identity, 9, gate, func(InspectionObservation) error { return nil })
	if err != nil || !observation.Matched || observation.Job.State != StateStashed {
		t.Fatalf("authorized staged inspection recovery failed: observation=%+v err=%v", observation, err)
	}
	if addHelp, enqueueHelp := len(stagedHelpInvocations(t, fake, "add")), len(stagedHelpInvocations(t, fake, "enqueue")); addHelp != 0 || enqueueHelp != 0 {
		t.Fatalf("recorded inspection recovery capability repeated help probes: add=%d enqueue=%d invocations=%+v", addHelp, enqueueHelp, readInvocations(t, fake.logPath))
	}
}

func TestSubmitStagedCommitsExactStashedReceiptBeforeEnqueue(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	_, permit, identity, launch := prepareStagedOrdinarySubmission(t, fake)
	var committed Observation
	observation, err := fake.client.SubmitStaged(context.Background(), permit, launch, captureOrdinaryStagedCommit(t, fake, identity, &committed))
	if err != nil {
		t.Fatal(err)
	}
	if observation != committed || observation.State != StateStashed || observation.NumericTaskID != 7 {
		t.Fatalf("staged ordinary observation changed unexpectedly: committed=%+v returned=%+v", committed, observation)
	}
	if !permit.IsConsumed() {
		t.Fatal("staged add did not consume the submission permit")
	}
	assertInitialOrdinaryStagedCommands(t, fake)
}

func TestSubmitStagedCommitFailureLeavesStashedWithoutEnqueue(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	_, permit, _, launch := prepareStagedOrdinarySubmission(t, fake)
	commitErr := errors.New("receipt write failed")
	observation, err := fake.client.SubmitStaged(context.Background(), permit, launch, func(candidate Observation) error {
		if candidate.State != StateStashed || candidate.NumericTaskID != 7 {
			t.Fatalf("commit received the wrong staged row: %+v", candidate)
		}
		return commitErr
	})
	if !errors.Is(err, commitErr) || observation.State != StateStashed || !observation.Matched {
		t.Fatalf("commit failure lost the Stashed receipt: observation=%+v err=%v", observation, err)
	}
	if got := len(stagedInvocations(t, fake, "add")); got != 1 || len(stagedInvocations(t, fake, "enqueue")) != 0 {
		t.Fatalf("commit failure changed scheduler state: adds=%d enqueues=%d", got, len(stagedInvocations(t, fake, "enqueue")))
	}
}

func TestSubmitStagedAddPendingIsConsumedWithoutDuplicateAdd(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	appendStagedEnvironment(fake, "FAKE_ADD_DELAY=0.05")
	fake.client.options.ObservationTimeout = 20 * time.Millisecond
	_, permit, _, launch := prepareStagedOrdinarySubmission(t, fake)
	observation, err := fake.client.SubmitStaged(context.Background(), permit, launch, func(Observation) error { return nil })
	if err != nil && (!errors.Is(err, ErrEnqueueUncertain) || observation.Pending == nil) {
		t.Fatalf("the original add Pending was not consumed: observation=%+v err=%v", observation, err)
	}
	if !observation.Matched {
		t.Fatalf("the original add Pending lost its staged receipt: observation=%+v err=%v", observation, err)
	}
	if observation.Pending != nil {
		result := waitPending(t, observation.Pending)
		if result.Err != nil || result.ExitCode != 0 {
			t.Fatalf("the single enqueue did not finish successfully: %+v", result)
		}
	}
	if got := len(stagedInvocations(t, fake, "add")); got != 1 {
		t.Fatalf("add was retried after its Pending: %d actual add invocations", got)
	}
	if got := len(stagedInvocations(t, fake, "enqueue")); got != 1 {
		t.Fatalf("successful staged add did not enqueue exactly once: %d", got)
	}
}

func TestSubmitStagedUnresolvedAddRetainsSubmissionPending(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	appendStagedEnvironment(fake, "FAKE_ADD_DELAY=0.5")
	fake.client.options.ObservationTimeout = 20 * time.Millisecond
	_, permit, _, launch := prepareStagedOrdinarySubmission(t, fake)
	observation, err := fake.client.SubmitStaged(context.Background(), permit, launch, func(Observation) error {
		t.Fatal("unresolved add reached application commit")
		return nil
	})
	if !errors.Is(err, ErrSubmissionUncertain) || !errors.Is(err, ErrInFlight) || observation.Matched || observation.Pending == nil {
		t.Fatalf("unresolved add was not reported safely: observation=%+v err=%v", observation, err)
	}
	if got := len(stagedInvocations(t, fake, "add")); got > 1 || len(stagedInvocations(t, fake, "enqueue")) != 0 {
		t.Fatalf("unresolved add was retried or enqueued while pending: adds=%d enqueues=%d", got, len(stagedInvocations(t, fake, "enqueue")))
	}
	if !awaitPendingNaturally(observation.Pending, 2*time.Second) {
		t.Fatal("unresolved add Pending did not settle")
	}
	if got := len(stagedInvocations(t, fake, "add")); got != 1 || len(stagedInvocations(t, fake, "enqueue")) != 0 {
		t.Fatalf("settled unresolved add was retried or enqueued: adds=%d enqueues=%d", got, len(stagedInvocations(t, fake, "enqueue")))
	}
}

func TestSubmitStagedEnqueueTimeoutRetainsReceiptAndPending(t *testing.T) {
	fake := newFakeSupervisor(t, "ok")
	appendStagedEnvironment(fake, "FAKE_ENQUEUE_DELAY=0.5")
	fake.client.options.ObservationTimeout = 20 * time.Millisecond
	_, permit, _, launch := prepareStagedOrdinarySubmission(t, fake)
	commitCalls := 0
	observation, err := fake.client.SubmitStaged(context.Background(), permit, launch, func(candidate Observation) error {
		commitCalls++
		if candidate.State != StateStashed || candidate.NumericTaskID != 7 {
			t.Fatalf("commit received the wrong staged row: %+v", candidate)
		}
		return nil
	})
	if !errors.Is(err, ErrEnqueueUncertain) || !errors.Is(err, ErrInFlight) || commitCalls != 1 {
		t.Fatalf("enqueue uncertainty was not separated from submission: observation=%+v err=%v commits=%d", observation, err, commitCalls)
	}
	if !observation.Matched || observation.State != StateStashed || observation.NumericTaskID != 7 || observation.Pending == nil {
		t.Fatalf("enqueue uncertainty lost the matched staged receipt: %+v", observation)
	}
	if !awaitPendingNaturally(observation.Pending, 2*time.Second) {
		t.Fatal("enqueue Pending did not settle")
	}
	if got := len(stagedInvocations(t, fake, "add")); got != 1 || len(stagedInvocations(t, fake, "enqueue")) != 1 {
		t.Fatalf("enqueue was duplicated or add retried: adds=%d enqueues=%d", got, len(stagedInvocations(t, fake, "enqueue")))
	}
}

func TestSubmitStagedExternalEnqueueRetainsEveryPrivateLease(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "delay-enqueue")
	configureExternalPendingEnqueue(t, fake)
	_, permit, _, launch := prepareStagedOrdinarySubmission(t, fake)
	clearStagedInvocationLog(t, fake)
	observation, err := fake.client.SubmitStaged(t.Context(), permit, launch, func(Observation) error { return nil })
	if !errors.Is(err, ErrEnqueueUncertain) || observation.Pending == nil {
		t.Fatalf("external ordinary enqueue did not remain pending: observation=%+v err=%v", observation, err)
	}
	assertNoStagedStatusSnapshot(t, fake)
	assertPendingSubmissionLeases(t, fake.client, store.Root, observation.Pending)
}

func TestSubmitConcurrentInspectionExternalEnqueueRetainsEveryPrivateLease(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "delay-enqueue")
	configureExternalPendingEnqueue(t, fake)
	identity := InspectionIdentity{RootID: store.RootID, TaskID: strings.Repeat("b", 32), QueueGroup: "default"}
	permit := &concurrentInspectionTestPermit{
		inspectionTestPermit: &inspectionTestPermit{
			submissionRoot:       identity.RootID,
			submissionTask:       identity.TaskID,
			submissionSupervisor: fake.client.Binding(),
		},
		queueGroup: "default",
	}
	launch := Launch{RunnerExecutable: fake.executable, RootPath: store.Root, Supervised: true}
	clearStagedInvocationLog(t, fake)
	observation, err := fake.client.SubmitConcurrentInspectionStaged(t.Context(), permit, identity, launch, func(InspectionObservation) error { return nil })
	if !errors.Is(err, ErrEnqueueUncertain) || observation.Pending == nil {
		t.Fatalf("external inspection enqueue did not remain pending: observation=%+v err=%v", observation, err)
	}
	assertNoStagedStatusSnapshot(t, fake)
	assertPendingSubmissionLeases(t, fake.client, store.Root, observation.Pending)
}

func configureExternalPendingEnqueue(t *testing.T, fake *fakeSupervisor) {
	t.Helper()
	owner, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	owner, err = filepath.EvalSymlinks(owner)
	if err != nil {
		t.Fatal(err)
	}
	fake.client.options.ControlOwnerExecutable = owner
	fake.client.options.Observer = func(event CommandEvent) {
		if event.Stage == "entry" && slices.Contains(event.Argv, "enqueue") && !slices.Contains(event.Argv, "--help") {
			fake.client.options.ObservationTimeout = 20 * time.Millisecond
		}
	}
}

func clearStagedInvocationLog(t *testing.T, fake *fakeSupervisor) {
	t.Helper()
	if err := os.WriteFile(fake.logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertNoStagedStatusSnapshot(t *testing.T, fake *fakeSupervisor) {
	t.Helper()
	if got := countCommand(readInvocations(t, fake.logPath), "status"); got != 0 {
		t.Fatalf("normal staged submission issued %d queue snapshots, want zero", got)
	}
}

func assertPendingSubmissionLeases(t *testing.T, client *Client, stateRoot string, pending *Pending) {
	t.Helper()
	bootstrapContext, cancelBootstrap := context.WithTimeout(t.Context(), 50*time.Millisecond)
	bootstrapErr := WithBootstrapLock(bootstrapContext, stateRoot, func() error { return nil })
	cancelBootstrap()
	if !errors.Is(bootstrapErr, context.DeadlineExceeded) {
		t.Fatalf("external enqueue released its supervisor generation: %v", bootstrapErr)
	}
	identityContext, cancelIdentity := context.WithTimeout(t.Context(), 50*time.Millisecond)
	identityErr := client.withPrivateQueueIdentityMutation(identityContext, func(context.Context) error { return nil })
	cancelIdentity()
	if !errors.Is(identityErr, context.DeadlineExceeded) {
		t.Fatalf("external enqueue released its queue identity lease: %v", identityErr)
	}
	if !awaitPendingNaturally(pending, 5*time.Second) {
		t.Fatal("external enqueue did not finish naturally")
	}
	result, ok := pending.Result()
	if !ok || result.Err != nil || result.ExitCode != 0 {
		t.Fatalf("external enqueue completed unsuccessfully: result=%+v available=%t", result, ok)
	}
	releaseContext, cancelRelease := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelRelease()
	if err := WithBootstrapLock(releaseContext, stateRoot, func() error { return nil }); err != nil {
		t.Fatalf("supervisor generation did not release after enqueue: %v", err)
	}
	if err := client.withPrivateQueueIdentityMutation(releaseContext, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("queue identity lease did not release after enqueue: %v", err)
	}
}
