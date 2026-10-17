package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

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
		func(_ context.Context, binding task.SupervisorRef) (SubmissionAuthorization, error) {
			gateCalls++
			if binding != fake.client.Binding() {
				t.Fatalf("inspection gate received the wrong binding: %+v", binding)
			}
			return SubmissionAuthorization{}, gateErr
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
