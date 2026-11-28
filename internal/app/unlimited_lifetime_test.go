package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	admissionfixture "github.com/hishamkaram/delegation-layer/internal/admission/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/guardian"
	guardiantest "github.com/hishamkaram/delegation-layer/internal/guardian/testsupport"
	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func newUnlimitedAppTask(t *testing.T) *taskdir.TaskDir {
	t.Helper()
	store, seed, _ := newAppTestTask(t, false)
	t.Cleanup(func() { closeAppTestTask(t, store, seed) })
	request, meta, err := seed.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	request.TaskID, meta.TaskID = strings.Repeat("d", 32), strings.Repeat("d", 32)
	request.RequestedConfig.Budget, request.BudgetNanos = "unlimited", 0
	meta.RequestedConfig = request.RequestedConfig
	td, err := store.CreateTask(request.TaskID, request, []byte("brief"), meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := td.Close(); err != nil {
			t.Error(err)
		}
	})
	return td
}

func TestUnlimitedGuardianProductionStoreClosesAndProvesTermination(t *testing.T) {
	td := newUnlimitedAppTask(t)
	request, _, err := td.PreparedRecords()
	if err != nil {
		t.Fatal(err)
	}
	fixture := guardiantest.NewRecordFixture(t, request.RootID, request.TaskID, task.LifetimeOwnerTask, time.Time{}, td,
		func(ctx context.Context) (guardian.Owner, error) { return td.AcquireLifetimeOwner(ctx) },
		func(options *guardian.Options) { options.Unlimited = true })
	fixture.Hello(t)
	fixture.RunCommand(t)
	fixture.CloseTask(t)
	fixture.ReleaseWorker()
	if err = fixture.WaitRun(t); err != nil {
		t.Fatal(err)
	}
	registration, err := td.ReadLifetimeRegistration()
	if err != nil {
		t.Fatal(err)
	}
	if !registration.Unlimited || registration.Deadline != "" {
		t.Fatal("guardian invented an execution deadline")
	}
	proof, err := guardian.ReadClosureProof(t.Context(), td)
	if err != nil || !proof.Closed() {
		t.Fatalf("closure=%v err=%v", proof.Closed(), err)
	}
	ready, err := td.StopTerminationReady()
	if err != nil || !ready {
		t.Fatalf("stop consumer ready=%t err=%v", ready, err)
	}
}

func TestUnlimitedRecoveryFailsClosedWithoutCooperativeLaunchProof(t *testing.T) {
	td := newUnlimitedAppTask(t)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	boot := admissionfixture.CurrentBootID(t)
	_, intentErr := td.RecordLifetimeIntent(taskdir.LifetimeIntentFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopePlan: "pg:" + strings.Repeat("a", 32), BootID: boot})
	_, registrationErr := td.RecordLifetimeRegistration(taskdir.LifetimeRegistrationFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopeID: "pg:" + strings.Repeat("a", 32), BootID: boot})
	if err := errors.Join(intentErr, registrationErr, owner.Release()); err != nil {
		t.Fatal(err)
	}
	if err := guardian.RecoverTaskLifetime(t.Context(), td); !errors.Is(err, lifetime.ErrUnverifiableLaunch) {
		t.Fatalf("unproven scope recovery=%v", err)
	}
}
