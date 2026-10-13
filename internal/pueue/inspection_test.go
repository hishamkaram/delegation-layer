package pueue

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
	"golang.org/x/sys/unix"
)

type inspectionTestPermit struct {
	calls                int
	err                  error
	groupRoot            string
	groupSupervisor      task.SupervisorRef
	submissionRoot       string
	submissionTask       string
	submissionSupervisor task.SupervisorRef
	stopRoot             string
	stopTask             string
	stopTarget           *int64
	stopSupervisor       task.SupervisorRef
}

func (p *inspectionTestPermit) consume() error {
	p.calls++
	return p.err
}

func (p *inspectionTestPermit) ConsumeInspectionGroup() error { return p.consume() }

func (p *inspectionTestPermit) InspectionGroupRootID() string { return p.groupRoot }

func (p *inspectionTestPermit) InspectionGroupSupervisor() task.SupervisorRef {
	return p.groupSupervisor
}

func (p *inspectionTestPermit) ConsumeInspectionSubmission() error { return p.consume() }

func (p *inspectionTestPermit) InspectionSubmissionRootID() string { return p.submissionRoot }

func (p *inspectionTestPermit) InspectionSubmissionTaskID() string { return p.submissionTask }

func (p *inspectionTestPermit) InspectionSubmissionSupervisor() task.SupervisorRef {
	return p.submissionSupervisor
}

func (p *inspectionTestPermit) ConsumeInspectionStop() error { return p.consume() }

func (p *inspectionTestPermit) InspectionStopRootID() string { return p.stopRoot }

func (p *inspectionTestPermit) InspectionStopTaskID() string { return p.stopTask }

func (p *inspectionTestPermit) InspectionStopNumericTaskID() *int64 {
	if p.stopTarget == nil {
		return nil
	}
	id := *p.stopTarget
	return &id
}

func (p *inspectionTestPermit) InspectionStopSupervisor() task.SupervisorRef { return p.stopSupervisor }

type concurrentInspectionTestPermit struct {
	*inspectionTestPermit
	queueGroup string
}

func (p *concurrentInspectionTestPermit) InspectionQueueGroup() string { return p.queueGroup }

type inspectionTestSubmissionPermit interface {
	InspectionSubmissionPermit
	inspectionCallCount() int
}

func (p *inspectionTestPermit) inspectionCallCount() int { return p.calls }

func TestInspectionIdentityDerivesExactGroupAndLabel(t *testing.T) {
	identity := InspectionIdentity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32)}
	if got, want := identity.Group(), "delegation-inspection-"+identity.RootID; got != want {
		t.Fatalf("wrong inspection group: got=%q want=%q", got, want)
	}
	if got, want := identity.Label(), "delegation-inspection-"+identity.RootID+"-"+identity.TaskID; got != want {
		t.Fatalf("wrong inspection label: got=%q want=%q", got, want)
	}
}

func TestMatchingInspectionObservationRequiresExactSingleSlotGroup(t *testing.T) {
	identity := InspectionIdentity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32)}
	label := identity.Label()
	base := QueueSnapshot{
		Jobs:   []Job{{ID: 7, Label: &label, State: StateQueued, Group: identity.Group()}},
		Groups: map[string]Group{identity.Group(): {Status: "Running", ParallelTasks: 1}},
	}
	if observation, err := matchingInspectionObservation(base, identity, validBindingForTest()); err != nil || !observation.Matched || observation.Job.ID != 7 {
		t.Fatalf("positive inspection match failed: %+v %v", observation, err)
	}

	target := int64(8)
	cases := map[string]QueueSnapshot{
		"missing group":     {Jobs: base.Jobs, Groups: map[string]Group{}},
		"paused group":      {Jobs: base.Jobs, Groups: map[string]Group{identity.Group(): {Status: "Paused", ParallelTasks: 1}}},
		"parallel group":    {Jobs: base.Jobs, Groups: map[string]Group{identity.Group(): {Status: "Running", ParallelTasks: 2}}},
		"wrong job group":   {Jobs: []Job{{ID: 7, Label: &label, State: StateQueued, Group: "default"}}, Groups: base.Groups},
		"unknown job state": {Jobs: []Job{{ID: 7, Label: &label, State: StateUnknown, Group: identity.Group()}}, Groups: base.Groups},
		"duplicate label":   {Jobs: append(slices.Clone(base.Jobs), Job{ID: 8, Label: &label, State: StateQueued, Group: identity.Group()}), Groups: base.Groups},
		"wrong target":      {Jobs: base.Jobs, Groups: base.Groups},
	}
	for name, snapshot := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := identity
			if name == "wrong target" {
				candidate.NumericTaskID = &target
			}
			observation, err := matchingInspectionObservation(snapshot, candidate, validBindingForTest())
			if !errors.Is(err, ErrUnknown) || observation.Matched {
				t.Fatalf("unsafe inspection observation accepted: %+v %v", observation, err)
			}
		})
	}
}

func TestInspectionIdentityValidationRejectsInvalidIDsAndNegativeTarget(t *testing.T) {
	cases := []InspectionIdentity{
		{RootID: "bad", TaskID: strings.Repeat("b", 32)},
		{RootID: strings.Repeat("a", 32), TaskID: "bad"},
	}
	negative := int64(-1)
	cases = append(cases, InspectionIdentity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32), NumericTaskID: &negative})
	for _, identity := range cases {
		if err := validateInspectionIdentity(identity); err == nil {
			t.Fatalf("invalid inspection identity accepted: %+v", identity)
		}
	}
}

func TestSnapshotVerifiesAndParsesInspectionGroup(t *testing.T) {
	fake := newInspectionSupervisor(t, "ok")
	identity := InspectionIdentity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32)}
	writeInspectionStatus(t, fake, identity, 7, StateQueued)
	snapshot, err := fake.client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Groups[identity.Group()] != (Group{Status: "Running", ParallelTasks: 1}) {
		t.Fatalf("inspection group was not retained: %+v", snapshot.Groups)
	}
	if len(snapshot.Jobs) != 1 || snapshot.Jobs[0].Group != identity.Group() {
		t.Fatalf("inspection job was not retained: %+v", snapshot.Jobs)
	}
	if countCommand(readInvocations(t, fake.logPath), "status") != 1 {
		t.Fatalf("snapshot did not issue one status command: %+v", readInvocations(t, fake.logPath))
	}
}

func TestReconcileInspectionUsesExactLabelStatusQuery(t *testing.T) {
	fake := newInspectionSupervisor(t, "ok")
	identity := InspectionIdentity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32)}
	target := int64(7)
	identity.NumericTaskID = &target
	writeInspectionStatus(t, fake, identity, target, StateEnded)

	observation, err := fake.client.ReconcileInspection(context.Background(), identity)
	if err != nil || !observation.Matched || observation.Job.ID != target || observation.Job.State != StateEnded || !observation.Job.Succeeded {
		t.Fatalf("exact inspection reconciliation failed: observation=%+v err=%v", observation, err)
	}

	var status []string
	for _, invocation := range readInvocations(t, fake.logPath) {
		if len(invocation) > 2 && invocation[2] == "status" {
			status = invocation
		}
	}
	want := []string{"-c", fake.configPath, "status", "--json", "label=" + identity.Label()}
	if !slices.Equal(status, want) {
		t.Fatalf("wrong exact inspection status argv: got=%q want=%q", status, want)
	}
}

func TestAwaitInspectionObservationConsumesPendingExactStatus(t *testing.T) {
	fake := newInspectionSupervisor(t, "ok")
	identity := InspectionIdentity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32)}
	target := int64(7)
	identity.NumericTaskID = &target
	writeInspectionStatus(t, fake, identity, target, StateEnded)
	pending := completedPendingInspectionObservation(t, fake, identity)

	observation, err := fake.client.AwaitInspectionObservation(t.Context(), identity, pending)
	if err != nil || !observation.Matched || observation.Job.ID != target || observation.Job.State != StateEnded || !observation.Job.Succeeded {
		t.Fatalf("pending exact observation was not consumed: observation=%+v err=%v", observation, err)
	}
	if count := countCommand(readInvocations(t, fake.logPath), "status"); count != 1 {
		t.Fatalf("pending observation launched another status command: count=%d", count)
	}
}

func TestInspectionObservationCommitCannotRaceSuccessorPublication(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	identity := InspectionIdentity{RootID: store.RootID, TaskID: strings.Repeat("b", 32)}
	target := int64(7)
	identity.NumericTaskID = &target
	writeInspectionStatus(t, fake, identity, target, StateEnded)
	pending := completedPendingInspectionObservation(t, fake, identity)

	committed := false
	successorPublished := false
	observation, err := fake.client.AwaitInspectionObservationWithLease(t.Context(), identity, pending, func(observation InspectionObservation) error {
		if !observation.Matched || observation.Job.State != StateEnded {
			return ErrUnknown
		}
		// A nonblocking exclusive attempt is the deterministic successor race:
		// it must remain outside the callback until the application commit is
		// complete.
		if lockErr := withPrivateLock(t.Context(), store.Root, "bootstrap.lock", unix.LOCK_EX|unix.LOCK_NB, false, func() error {
			successorPublished = true
			return nil
		}); lockErr != nil {
			return lockErr
		}
		if successorPublished {
			return ErrBinding
		}
		committed = true
		return nil
	})
	if err != nil || !observation.Matched || !committed || successorPublished {
		t.Fatalf("inspection commit was not protected by its generation lease: observation=%+v committed=%t successor=%t err=%v", observation, committed, successorPublished, err)
	}
	if err = WithBootstrapLock(t.Context(), store.Root, func() error {
		if !committed {
			return ErrBinding
		}
		successorPublished = true
		return nil
	}); err != nil || !successorPublished {
		t.Fatalf("successor publication did not wait for the inspection commit: published=%t err=%v", successorPublished, err)
	}
}

func TestCachedInspectionObservationCommitCannotRaceSuccessorPublication(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	identity := InspectionIdentity{RootID: store.RootID, TaskID: strings.Repeat("b", 32)}
	target := int64(7)
	identity.NumericTaskID = &target
	writeInspectionStatus(t, fake, identity, target, StateEnded)
	label := identity.Label()
	observation := InspectionObservation{
		Binding:  fake.client.Binding(),
		Identity: identity,
		Job: Job{
			ID:        target,
			Label:     &label,
			State:     StateEnded,
			Group:     identity.Group(),
			Succeeded: true,
		},
		Matched: true,
	}

	committed := false
	successorPublished := false
	err := fake.client.CommitInspectionObservation(t.Context(), observation, func(observation InspectionObservation) error {
		if !observation.Matched || observation.Job.State != StateEnded {
			return ErrUnknown
		}
		if lockErr := withPrivateLock(t.Context(), store.Root, "bootstrap.lock", unix.LOCK_EX|unix.LOCK_NB, false, func() error {
			successorPublished = true
			return nil
		}); lockErr != nil {
			return lockErr
		}
		if successorPublished {
			return ErrBinding
		}
		committed = true
		return nil
	})
	if err != nil || !committed || successorPublished {
		t.Fatalf("cached inspection commit was not protected by its generation lease: committed=%t successor=%t err=%v", committed, successorPublished, err)
	}
	if err = WithBootstrapLock(t.Context(), store.Root, func() error {
		if !committed {
			return ErrBinding
		}
		successorPublished = true
		return nil
	}); err != nil || !successorPublished {
		t.Fatalf("successor publication did not wait for the cached inspection commit: published=%t err=%v", successorPublished, err)
	}
}

func TestAwaitInspectionObservationRejectsPendingFromAnotherIdentityOrGeneration(t *testing.T) {
	for _, name := range []string{"identity", "generation"} {
		t.Run(name, func(t *testing.T) {
			fake := newInspectionSupervisor(t, "ok")
			identity := InspectionIdentity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32)}
			target := int64(7)
			identity.NumericTaskID = &target
			writeInspectionStatus(t, fake, identity, target, StateEnded)
			pending := completedPendingInspectionObservation(t, fake, identity)

			client, candidate := fake.client, identity
			if name == "identity" {
				candidate.TaskID = strings.Repeat("c", 32)
			} else {
				successor := *fake.client
				successor.runtimeBinding.ClientSHA256 = strings.Repeat("c", 64)
				client = &successor
			}
			observation, err := client.AwaitInspectionObservation(t.Context(), candidate, pending)
			if !errors.Is(err, task.ErrIdentityMismatch) || observation.Matched {
				t.Fatalf("mismatched pending observation was accepted: observation=%+v err=%v", observation, err)
			}
			if count := countCommand(readInvocations(t, fake.logPath), "status"); count != 1 {
				t.Fatalf("pending mismatch launched another status command: count=%d", count)
			}
		})
	}
}

func TestAwaitInspectionObservationRejectsGenerationReplacementAfterCompletion(t *testing.T) {
	fake := newInspectionSupervisor(t, "ok")
	identity := InspectionIdentity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32)}
	target := int64(7)
	identity.NumericTaskID = &target
	writeInspectionStatus(t, fake, identity, target, StateEnded)
	pending := completedPendingInspectionObservation(t, fake, identity)

	executable, err := os.ReadFile(fake.executable)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(fake.executable, append(executable, '\n'), 0o700); err != nil {
		t.Fatal(err)
	}
	observation, err := fake.client.AwaitInspectionObservation(t.Context(), identity, pending)
	if !errors.Is(err, ErrBinding) || observation.Matched {
		t.Fatalf("completed observation crossed a generation replacement: observation=%+v err=%v", observation, err)
	}
	if count := countCommand(readInvocations(t, fake.logPath), "status"); count != 1 {
		t.Fatalf("generation replacement launched another status command: count=%d", count)
	}
}

func completedPendingInspectionObservation(t *testing.T, fake *fakeSupervisor, identity InspectionIdentity) *Pending {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	fake.client.options.Observer = func(event CommandEvent) {
		if event.Stage == "completed" && slices.Contains(event.Argv, "status") {
			cancel()
			<-release
		}
	}
	observation, err := fake.client.ReconcileInspection(ctx, identity)
	pending := pendingFrom(err)
	if pending == nil || observation.Pending != pending {
		close(release)
		t.Fatalf("exact observation did not retain its result owner: observation=%+v err=%v", observation, err)
	}
	close(release)
	<-pending.Done()
	return pending
}

func TestReconcileInspectionDoesNotParseTheRetainedQueue(t *testing.T) {
	fake := newInspectionSupervisor(t, "ok")
	identity := InspectionIdentity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32)}
	target := int64(7)
	identity.NumericTaskID = &target

	large, _ := statusStreamTestLargeQueue(t, 0)
	filteredJob := statusTestJob(target, identity.Label(), statusStreamTestDoneState("Success"))
	filteredJob["group"] = identity.Group()
	filtered := statusTestPayload(t, map[string]any{
		strconv.FormatInt(target, 10): filteredJob,
	}, map[string]any{
		identity.Group(): map[string]any{"status": "Running", "parallel_tasks": 1},
	})
	if len(large) <= MaxControlBytes {
		t.Fatalf("retained queue fixture has only %d bytes", len(large))
	}
	filteredPath := filepath.Join(filepath.Dir(fake.statusPath), "filtered-status.json")
	if err := os.WriteFile(fake.statusPath, large, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filteredPath, filtered, 0o600); err != nil {
		t.Fatal(err)
	}
	fake.client.options.Environment = append(fake.client.options.Environment, "FAKE_FILTERED_STATUS="+filteredPath)

	observation, err := fake.client.ReconcileInspection(context.Background(), identity)
	if err != nil || !observation.Matched || observation.Job.ID != target || !observation.Job.Succeeded {
		t.Fatalf("reconciliation did not use bounded exact status: observation=%+v err=%v", observation, err)
	}
}

func TestCreateInspectionGroupUsesDerivedLiteralAndOnePermit(t *testing.T) {
	fake := newInspectionSupervisor(t, "ok")
	rootID := strings.Repeat("a", 32)
	permit := &inspectionTestPermit{groupRoot: rootID, groupSupervisor: fake.client.Binding()}
	result, err := fake.client.CreateInspectionGroup(context.Background(), permit, rootID)
	if err != nil || result.ExitCode != 0 || permit.calls != 1 {
		t.Fatalf("group creation failed: result=%+v err=%v permit_calls=%d", result, err, permit.calls)
	}
	var group []string
	for _, invocation := range readInvocations(t, fake.logPath) {
		if len(invocation) > 2 && invocation[2] == "group" {
			group = invocation
		}
	}
	want := []string{"-c", fake.configPath, "group", "add", "--parallel", "1", inspectionGroup(rootID)}
	if !slices.Equal(group, want) {
		t.Fatalf("wrong group argv: got=%q want=%q", group, want)
	}
}

func TestInspectionPermitAcceptsSupervisorInstallProvenanceDrift(t *testing.T) {
	fake := newInspectionSupervisor(t, "ok")
	candidate := fake.client.Binding()
	candidate.ClientExecutable = "/old/install/pueue"
	candidate.ClientSHA256 = task.ComputeSHA256([]byte("old client"))
	candidate.ObservedVersion = "pueue " + FixtureVersion
	if err := validateInspectionPermitSupervisor(candidate, fake.client.Binding()); err != nil {
		t.Fatalf("same private supervisor identity rejected after install provenance changed: %v", err)
	}
}

func TestCreateInspectionGroupRejectsPermitIdentityAndBindingBeforeObservation(t *testing.T) {
	rootID := strings.Repeat("a", 32)
	for _, testCase := range []struct {
		name   string
		mutate func(*inspectionTestPermit, task.SupervisorRef)
		want   error
	}{
		{
			name: "wrong root",
			mutate: func(permit *inspectionTestPermit, _ task.SupervisorRef) {
				permit.groupRoot = strings.Repeat("b", 32)
			},
			want: task.ErrIdentityMismatch,
		},
		{
			name: "wrong supervisor",
			mutate: func(permit *inspectionTestPermit, binding task.SupervisorRef) {
				binding.ConfigDigest = task.ComputeSHA256([]byte("wrong-config"))
				permit.groupSupervisor = binding
			},
			want: ErrBinding,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newInspectionSupervisor(t, "ok")
			permit := &inspectionTestPermit{groupRoot: rootID, groupSupervisor: fake.client.Binding()}
			testCase.mutate(permit, fake.client.Binding())
			if _, err := fake.client.CreateInspectionGroup(context.Background(), permit, rootID); !errors.Is(err, testCase.want) {
				t.Fatalf("unsafe group permit was accepted: %v", err)
			}
			if permit.calls != 0 || inspectionMutationCount(t, fake) != 0 {
				t.Fatalf("group permit mismatch consumed or observed supervisor: calls=%d invocations=%+v", permit.calls, readInvocations(t, fake.logPath))
			}
		})
	}
}

func TestCreateInspectionGroupRejectsInvalidRootBeforeMutation(t *testing.T) {
	fake := newInspectionSupervisor(t, "ok")
	permit := &inspectionTestPermit{}
	if _, err := fake.client.CreateInspectionGroup(context.Background(), permit, "invalid"); !errors.Is(err, task.ErrInvalidRootID) {
		t.Fatalf("invalid root was accepted: %v", err)
	}
	if permit.calls != 0 || countCommand(readInvocations(t, fake.logPath), "group") != 0 {
		t.Fatalf("invalid root consumed or mutated: calls=%d invocations=%+v", permit.calls, readInvocations(t, fake.logPath))
	}
}

func TestSubmitInspectionUsesFixedWorkerArgumentsAndReconciles(t *testing.T) {
	fake := newInspectionSupervisor(t, "ok")
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
	identity := InspectionIdentity{RootID: record.RootID, TaskID: record.TaskID}
	writeInspectionStatus(t, fake, identity, 9, StateQueued)
	permit := &inspectionTestPermit{
		submissionRoot:       identity.RootID,
		submissionTask:       identity.TaskID,
		submissionSupervisor: fake.client.Binding(),
	}
	observation, err := fake.client.SubmitInspection(context.Background(), permit, identity, Launch{RunnerExecutable: fake.executable, RootPath: fixture.store.Root})
	if err != nil {
		t.Fatal(err)
	}
	if permit.calls != 1 || !observation.Matched || observation.Job.ID != 9 || observation.Identity.NumericTaskID == nil || *observation.Identity.NumericTaskID != 9 {
		t.Fatalf("inspection admission was not reconciled: observation=%+v permit_calls=%d", observation, permit.calls)
	}
	var add []string
	for _, invocation := range readInvocations(t, fake.logPath) {
		if len(invocation) > 2 && invocation[2] == "add" {
			add = invocation
		}
	}
	want := []string{"-c", fake.configPath, "add", "--escape", "--group", identity.Group(), "--label", identity.Label(), "--print-task-id", "--", fake.executable, "--inspection", "--root", fixture.store.Root, identity.TaskID}
	if !slices.Equal(add, want) {
		t.Fatalf("wrong inspection worker argv: got=%q want=%q", add, want)
	}
	if countCommand(readInvocations(t, fake.logPath), "add") != 1 {
		t.Fatalf("inspection admission was retried: %+v", readInvocations(t, fake.logPath))
	}
}

func TestSubmitConcurrentInspectionUsesAddReceiptWithoutQueueSnapshot(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	if err := os.WriteFile(fake.logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := InspectionIdentity{RootID: store.RootID, TaskID: strings.Repeat("b", 32), QueueGroup: "default"}
	permit := &concurrentInspectionTestPermit{inspectionTestPermit: &inspectionTestPermit{
		submissionRoot:       identity.RootID,
		submissionTask:       identity.TaskID,
		submissionSupervisor: fake.client.Binding(),
	}, queueGroup: "default"}
	observation, err := fake.client.SubmitConcurrentInspection(context.Background(), permit, identity, Launch{RunnerExecutable: fake.executable, RootPath: store.Root, Supervised: true})
	if err != nil {
		t.Fatal(err)
	}
	assertConcurrentInspectionAddReceipt(t, fake, store, identity, permit, observation)
}

func TestSubmitConcurrentInspectionCarriesTerminalEventRegistrationFailure(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	if err := os.WriteFile(filepath.Join(store.Root, privateSupervisorDirectory, privateTerminalEventTicketDir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := InspectionIdentity{RootID: store.RootID, TaskID: strings.Repeat("b", 32), QueueGroup: "default"}
	permit := &concurrentInspectionTestPermit{inspectionTestPermit: &inspectionTestPermit{
		submissionRoot:       identity.RootID,
		submissionTask:       identity.TaskID,
		submissionSupervisor: fake.client.Binding(),
	}, queueGroup: "default"}
	observation, err := fake.client.SubmitConcurrentInspection(context.Background(), permit, identity, Launch{RunnerExecutable: fake.executable, RootPath: store.Root, Supervised: true})
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Matched || !observation.TerminalEventRegistrationKnown || observation.TerminalEventRegistered {
		t.Fatalf("failed terminal-event registration was not carried as per-submission state: %+v", observation)
	}
	if permit.calls != 1 || countCommand(readInvocations(t, fake.logPath), "add") != 1 {
		t.Fatalf("failed terminal-event registration changed create-once submission: calls=%d invocations=%+v", permit.calls, readInvocations(t, fake.logPath))
	}
}

func TestSubmitConcurrentInspectionWithGateRejectsBeforeAddOrPermitConsumption(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	if err := os.WriteFile(fake.logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := InspectionIdentity{RootID: store.RootID, TaskID: strings.Repeat("b", 32), QueueGroup: "default"}
	permit := &concurrentInspectionTestPermit{inspectionTestPermit: &inspectionTestPermit{
		submissionRoot:       identity.RootID,
		submissionTask:       identity.TaskID,
		submissionSupervisor: fake.client.Binding(),
	}, queueGroup: "default"}
	gateErr := errors.New("inspection generation drift")
	gateCalls := 0
	observation, err := fake.client.SubmitConcurrentInspectionWithGate(
		context.Background(),
		permit,
		identity,
		Launch{RunnerExecutable: fake.executable, RootPath: store.Root, Supervised: true},
		func(_ context.Context, binding task.SupervisorRef) error {
			gateCalls++
			if binding != fake.client.Binding() {
				t.Fatalf("inspection gate received the wrong binding: %+v", binding)
			}
			return gateErr
		},
	)
	if !errors.Is(err, gateErr) || observation.Matched {
		t.Fatalf("inspection gate refusal was not returned before admission: observation=%+v err=%v", observation, err)
	}
	if gateCalls != 1 {
		t.Fatalf("inspection gate calls=%d, want one", gateCalls)
	}
	if permit.calls != 0 {
		t.Fatalf("inspection permit consumed before add: calls=%d", permit.calls)
	}
	if countCommand(readInvocations(t, fake.logPath), "add") != 0 {
		t.Fatal("inspection gate refusal reached the pueue add command")
	}
}

func assertConcurrentInspectionAddReceipt(t *testing.T, fake *fakeSupervisor, store *taskdir.Store, identity InspectionIdentity, permit *concurrentInspectionTestPermit, observation InspectionObservation) {
	t.Helper()
	if permit.calls != 1 {
		t.Fatalf("concurrent inspection permit calls=%d, want one", permit.calls)
	}
	if !observation.Matched || observation.Job.ID != 9 || observation.Job.State != StateQueued || observation.Job.Group != "default" {
		t.Fatalf("concurrent inspection admission did not retain its add receipt: observation=%+v", observation)
	}
	if observation.Identity.NumericTaskID == nil || *observation.Identity.NumericTaskID != 9 {
		t.Fatalf("concurrent inspection admission lost its numeric receipt: observation=%+v", observation)
	}
	if !observation.TerminalEventRegistrationKnown || !observation.TerminalEventRegistered {
		t.Fatalf("concurrent inspection admission lost terminal event registration state: observation=%+v", observation)
	}
	assertConcurrentInspectionCommands(t, fake, store, identity)
}

func assertConcurrentInspectionCommands(t *testing.T, fake *fakeSupervisor, store *taskdir.Store, identity InspectionIdentity) {
	t.Helper()
	invocations := readInvocations(t, fake.logPath)
	if got := countCommand(invocations, "status"); got != 0 {
		t.Fatalf("concurrent inspection took %d queue-wide status snapshots: %+v", got, invocations)
	}
	if got := countCommand(invocations, "add"); got != 1 {
		t.Fatalf("concurrent inspection did not submit exactly one worker: %+v", invocations)
	}
	var add []string
	for _, invocation := range invocations {
		if len(invocation) > 2 && invocation[2] == "add" {
			add = invocation
		}
	}
	want := []string{"-c", fake.configPath, "add", "--escape", "--group", "default", "--label", identity.Label(), "--print-task-id", "--", fake.executable, "--inspection", "--guardian", "--supervised", "--root", store.Root, identity.TaskID}
	if !slices.Equal(add, want) {
		t.Fatalf("wrong concurrent inspection argv: got=%q want=%q", add, want)
	}
}

func TestSubmitConcurrentInspectionRejectsIneligibleRouteBeforeSupervisorCommand(t *testing.T) {
	t.Run("named group", testSubmitConcurrentInspectionRejectsNamedGroup)
	t.Run("external default group", testSubmitConcurrentInspectionRejectsExternalDefaultGroup)
	t.Run("different private root", testSubmitConcurrentInspectionRejectsDifferentPrivateRoot)
}

func assertConcurrentInspectionRejected(t *testing.T, fake *fakeSupervisor, permit inspectionTestSubmissionPermit, identity InspectionIdentity, launch Launch, wantErr error) {
	t.Helper()
	if _, err := fake.client.SubmitConcurrentInspection(context.Background(), permit, identity, launch); !errors.Is(err, wantErr) {
		t.Fatalf("concurrent route returned %v, want %v", err, wantErr)
	}
	invocations := readInvocations(t, fake.logPath)
	if permit.inspectionCallCount() != 0 || len(invocations) != 0 {
		t.Fatalf("concurrent route consumed authority or contacted the supervisor: calls=%d invocations=%+v", permit.inspectionCallCount(), invocations)
	}
}

func testSubmitConcurrentInspectionRejectsNamedGroup(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	if err := os.WriteFile(fake.logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := InspectionIdentity{RootID: store.RootID, TaskID: strings.Repeat("b", 32)}
	permit := &inspectionTestPermit{
		submissionRoot:       identity.RootID,
		submissionTask:       identity.TaskID,
		submissionSupervisor: fake.client.Binding(),
	}
	assertConcurrentInspectionRejected(t, fake, permit, identity, Launch{RunnerExecutable: fake.executable, RootPath: store.Root}, task.ErrIdentityMismatch)
}

func testSubmitConcurrentInspectionRejectsExternalDefaultGroup(t *testing.T) {
	fake := newInspectionSupervisor(t, "ok")
	store, err := taskdir.InitStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = os.WriteFile(fake.logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := InspectionIdentity{RootID: store.RootID, TaskID: strings.Repeat("b", 32), QueueGroup: "default"}
	permit := &concurrentInspectionTestPermit{inspectionTestPermit: &inspectionTestPermit{
		submissionRoot:       identity.RootID,
		submissionTask:       identity.TaskID,
		submissionSupervisor: fake.client.Binding(),
	}, queueGroup: "default"}
	assertConcurrentInspectionRejected(t, fake, permit, identity, Launch{RunnerExecutable: fake.executable, RootPath: store.Root}, ErrConfiguration)
}

func testSubmitConcurrentInspectionRejectsDifferentPrivateRoot(t *testing.T) {
	fake, store := newPrivateInspectionSupervisor(t, "ok")
	other, err := taskdir.InitStore(filepath.Join(canonicalTemp(t), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := other.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = os.WriteFile(fake.logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	identity := InspectionIdentity{RootID: other.RootID, TaskID: strings.Repeat("b", 32), QueueGroup: "default"}
	permit := &concurrentInspectionTestPermit{inspectionTestPermit: &inspectionTestPermit{
		submissionRoot:       identity.RootID,
		submissionTask:       identity.TaskID,
		submissionSupervisor: fake.client.Binding(),
	}, queueGroup: "default"}
	assertConcurrentInspectionRejected(t, fake, permit, identity, Launch{RunnerExecutable: fake.executable, RootPath: other.Root}, task.ErrIdentityMismatch)
	if store.Root == other.Root {
		t.Fatal("test did not construct distinct private roots")
	}
}

func TestSubmitInspectionRejectsPermitBindingsBeforeObservation(t *testing.T) {
	identity := InspectionIdentity{RootID: strings.Repeat("a", 32), TaskID: strings.Repeat("b", 32)}
	for _, testCase := range []struct {
		name   string
		mutate func(*inspectionTestPermit, *InspectionIdentity, task.SupervisorRef)
		want   error
	}{
		{
			name: "wrong root",
			mutate: func(permit *inspectionTestPermit, _ *InspectionIdentity, _ task.SupervisorRef) {
				permit.submissionRoot = strings.Repeat("c", 32)
			},
			want: task.ErrIdentityMismatch,
		},
		{
			name: "wrong task",
			mutate: func(permit *inspectionTestPermit, _ *InspectionIdentity, _ task.SupervisorRef) {
				permit.submissionTask = strings.Repeat("c", 32)
			},
			want: task.ErrIdentityMismatch,
		},
		{
			name: "unexpected target",
			mutate: func(_ *inspectionTestPermit, candidate *InspectionIdentity, _ task.SupervisorRef) {
				id := int64(7)
				candidate.NumericTaskID = &id
			},
			want: task.ErrIdentityMismatch,
		},
		{
			name: "wrong supervisor",
			mutate: func(permit *inspectionTestPermit, _ *InspectionIdentity, binding task.SupervisorRef) {
				binding.ConfigDigest = task.ComputeSHA256([]byte("wrong-config"))
				permit.submissionSupervisor = binding
			},
			want: ErrBinding,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newInspectionSupervisor(t, "ok")
			candidate := identity
			permit := &inspectionTestPermit{
				submissionRoot:       identity.RootID,
				submissionTask:       identity.TaskID,
				submissionSupervisor: fake.client.Binding(),
			}
			testCase.mutate(permit, &candidate, fake.client.Binding())
			if _, err := fake.client.SubmitInspection(context.Background(), permit, candidate, Launch{}); !errors.Is(err, testCase.want) {
				t.Fatalf("unsafe submission permit was accepted: %v", err)
			}
			if permit.calls != 0 || inspectionMutationCount(t, fake) != 0 {
				t.Fatalf("submission permit mismatch consumed or observed supervisor: calls=%d invocations=%+v", permit.calls, readInvocations(t, fake.logPath))
			}
		})
	}
}

type inspectionStopCase struct {
	name         string
	mode         string
	state        State
	action       string
	acknowledged *bool
}

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

const inspectionSupervisorScript = `#!/bin/sh
set -eu
{
  printf 'argc=%s\n' "$#"
  for arg in "$@"; do printf 'arg=%s\n' "$arg"; done
  printf '%s\n' '---'
} >> "$FAKE_LOG"
case "${3-}" in
  --version)
    printf '%s\n' "${FAKE_VERSION:-pueue 4.0.4}"
    ;;
  status)
    if [ "${5-}" != "" ] && [ "${FAKE_FILTERED_STATUS:-}" != "" ]; then
      cat "$FAKE_FILTERED_STATUS"
    else
      cat "$FAKE_STATUS"
    fi
    ;;
  group)
    [ "${4-}" = add ] && [ "${5-}" = --parallel ] && [ "${6-}" = 1 ]
    ;;
  add)
    if [ "${4-}" = "--help" ]; then
      printf '%s\n' 'Usage: pueue add [OPTIONS] <COMMAND>...'
      printf '%s\n' '  -s, --stashed    Create the task in Stashed state.'
      printf '%s\n' '  -p, --print-task-id    Only return the task id.'
    else
      printf '%s\n' "${FAKE_ADD_ID:-9}"
    fi
    ;;
  enqueue)
    if [ "${4-}" = "--help" ]; then
      printf '%s\n' 'Usage: pueue enqueue <TASK_IDS>...'
    else
      case "${FAKE_MODE:-ok}" in
        enqueue-refuse) printf '%s\n' 'refused' >&2; exit 7 ;;
        *) exit 0 ;;
      esac
    fi
    ;;
  remove|kill)
    case "${FAKE_MODE:-ok}" in
      stop-refuse) printf '%s\n' 'refused' >&2; exit 7 ;;
      stop-overflow) dd if=/dev/zero bs=1048577 count=1 2>/dev/null ;;
      *) exit 0 ;;
    esac
    ;;
  *)
    printf '%s\n' 'unexpected command' >&2
    exit 64
    ;;
esac
`
