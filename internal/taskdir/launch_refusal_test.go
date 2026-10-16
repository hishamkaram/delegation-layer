package taskdir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPreparedUserStopRefusesEveryFreshTaskLaunch(t *testing.T) {
	store := testStore(t)
	td := supervisorTask(t, store)
	_, meta, err := td.PreparedRecords()
	must(t, err)
	permit, err := td.PrepareStop(strings.Repeat("d", 32), "user", time.Time{})
	must(t, err)
	request, err := permit.Request()
	must(t, err)
	must(t, permit.Consume())
	must(t, permit.Release())
	if request.NumericTaskID != nil || request.Supervisor != meta.SupervisorConfig {
		t.Fatalf("unsubmitted cancellation fabricated a target: %+v", request)
	}
	if _, err = td.PrepareSubmission(meta.SupervisorConfig); !errors.Is(err, task.ErrTerminalTask) {
		t.Fatalf("submission after user stop = %v", err)
	}
	if _, err = td.PrepareStart(0); !errors.Is(err, task.ErrTerminalTask) {
		t.Fatalf("provider start after user stop = %v", err)
	}
	if _, err = td.RecordLifetimeIntentContext(t.Context(), LifetimeIntentFacts{ScopeKind: task.LifetimeScopeProcessGroup, Assurance: task.LifetimeAssuranceCooperative, ScopePlan: "late-task-plan", BootID: "late-boot", Deadline: time.Now().Add(time.Hour)}); !errors.Is(err, task.ErrTerminalTask) {
		t.Fatalf("scope intent after user stop = %v", err)
	}
	proven, err := td.ProveUnlaunchedTaskContext(t.Context())
	must(t, err)
	if !proven {
		t.Fatal("durably refused task with no native or provider start facts was not recognized")
	}
	for _, name := range []string{"provider.start", "provider.exit", "outcome.json", filepath.Join(lifetimeDirectory, lifetimeIntentName), filepath.Join(lifetimeDirectory, lifetimeTerminationName), filepath.Join(lifetimeDirectory, lifetimeWorkerWaitName)} {
		if _, err = os.Lstat(filepath.Join(td.Dir, name)); !os.IsNotExist(err) {
			t.Fatalf("unlaunched proof fabricated %s: %v", name, err)
		}
	}
}

func TestObservedUserStopRemainsARefusal(t *testing.T) {
	store := testStore(t)
	td := supervisorTask(t, store)
	supervisorSubmit(t, td)
	supervisorReceipt(t, td)
	id := strings.Repeat("d", 32)
	permit, err := td.PrepareStop(id, "user", time.Time{})
	must(t, err)
	must(t, permit.Consume())
	must(t, permit.Release())
	must(t, td.RecordStopObservationContext(t.Context(), id, task.StopObservationFacts{NumericTaskID: 7, State: "ended", Terminated: true}))
	requested, err := td.UserStopRequestedContext(t.Context())
	must(t, err)
	refused, err := td.TaskLaunchRefusedContext(t.Context())
	must(t, err)
	if !requested || !refused {
		t.Fatal("observing a user stop revoked its launch refusal")
	}
}

func TestUnlaunchedTaskProofRequiresDurableRefusalAndIdleOwner(t *testing.T) {
	store := testStore(t)
	td := preparedTask(t, store)
	proven, err := td.ProveUnlaunchedTaskContext(t.Context())
	must(t, err)
	if proven {
		t.Fatal("absence of native records alone authorized task closure")
	}
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	t.Cleanup(func() { must(t, owner.Release()) })
	proven, err = td.ProveUnlaunchedTaskContext(t.Context())
	must(t, err)
	if proven {
		t.Fatal("active lifetime owner authorized task closure")
	}
}
