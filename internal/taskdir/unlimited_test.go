package taskdir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func preparedUnlimitedTask(t *testing.T) *TaskDir {
	t.Helper()
	store := testStore(t)
	request, meta, brief := preparedInput(t, store)
	request.RequestedConfig.Budget, request.BudgetNanos = "unlimited", 0
	meta.RequestedConfig = request.RequestedConfig
	meta.SupervisorConfig.ClientExecutable = "/fake/pueue"
	meta.SupervisorConfig.ClientSHA256 = task.ComputeSHA256([]byte("client"))
	meta.SupervisorConfig.ResolvedConfigSHA256 = task.ComputeSHA256([]byte("resolved"))
	directory, err := store.CreateTask(request.TaskID, request, brief, meta)
	must(t, err)
	t.Cleanup(func() { must(t, directory.Close()) })
	return directory
}

func TestUnlimitedTaskStartAndUserStopRetainBudgetBinding(t *testing.T) {
	directory := preparedUnlimitedTask(t)
	start, err := directory.PrepareStart(0)
	must(t, err)
	record, err := start.Record()
	must(t, err)
	if !record.BudgetUnlimited || record.BudgetNanos != 0 {
		t.Fatal("start permit dropped the unlimited budget binding")
	}
	must(t, start.Release())
	_, _, err = directory.PreparedRecords()
	must(t, err)
	stop, err := directory.PrepareStop("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "user", time.Time{})
	must(t, err)
	must(t, stop.Release())
	request, err := directory.ReadStopRequest("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	must(t, err)
	if !request.BudgetUnlimited || request.BudgetNanos != 0 || request.Deadline != "" {
		t.Fatal("user cancellation lost the unlimited budget binding")
	}
}

func TestUnlimitedLifetimeRecordsDeriveNoDeadlineFromTask(t *testing.T) {
	directory := preparedUnlimitedTask(t)
	owner, err := directory.AcquireLifetimeOwner(t.Context())
	must(t, err)
	t.Cleanup(func() { must(t, owner.Release()) })
	intent, err := directory.RecordLifetimeIntent(LifetimeIntentFacts{ScopeKind: task.LifetimeScopeCgroupV2, Assurance: task.LifetimeAssuranceExact, ScopePlan: "unlimited-test-plan", BootID: "test-boot"})
	must(t, err)
	registration, err := directory.RecordLifetimeRegistration(LifetimeRegistrationFacts{ScopeKind: task.LifetimeScopeCgroupV2, Assurance: task.LifetimeAssuranceExact, ScopeID: "/sys/fs/cgroup/unlimited-test", BootID: "test-boot"})
	must(t, err)
	if !intent.Unlimited || intent.Deadline != "" || !registration.Unlimited || registration.Deadline != "" {
		t.Fatal("lifetime records invented a finite execution deadline")
	}
	_, _, err = directory.PreparedRecords()
	must(t, err)
}

func TestUnlimitedLifetimeReaderRejectsConflictingFiniteProjection(t *testing.T) {
	directory := preparedUnlimitedTask(t)
	owner, err := directory.AcquireLifetimeOwner(t.Context())
	must(t, err)
	t.Cleanup(func() { must(t, owner.Release()) })
	intent, err := directory.RecordLifetimeIntent(LifetimeIntentFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopePlan: "pg:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BootID: "test-boot"})
	must(t, err)
	intent.Unlimited, intent.Deadline = false, time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	data, err := task.MarshalCanonical(intent)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(directory.Dir, lifetimeDirectory, lifetimeIntentName), data, 0o600))
	if _, err := directory.ReadLifetimeIntent(); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("conflicting execution projection: %v", err)
	}
}
