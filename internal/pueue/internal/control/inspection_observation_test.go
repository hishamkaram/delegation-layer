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
	"golang.org/x/sys/unix"
)

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
func (p *concurrentInspectionTestPermit) InspectionQueueGroup() string       { return p.queueGroup }
func (p *inspectionTestPermit) inspectionCallCount() int                     { return p.calls }
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
