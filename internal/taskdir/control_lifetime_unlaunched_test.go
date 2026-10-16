package taskdir

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestControlLifetimeUnlaunchedDeadlineAndImmutableReplay(t *testing.T) {
	deadline := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	_, _, lifetime := newWorkspaceControlLifetime(t, deadline)

	before := deadline.Add(-time.Nanosecond)
	if record, err := lifetime.sealLifetimeUnlaunchedContextWithNow(t.Context(), func() time.Time { return before }); record != nil || !errors.Is(err, task.ErrLifetimeDeadlineNotExpired) {
		t.Fatalf("seal before deadline = (%+v, %v), want nil/ErrLifetimeDeadlineNotExpired", record, err)
	}
	if _, err := lifetime.ReadLifetimeUnlaunchedContext(t.Context()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fence after pre-deadline refusal = %v, want absence", err)
	}

	observed := deadline.Add(time.Second)
	first, err := lifetime.sealLifetimeUnlaunchedContextWithNow(t.Context(), func() time.Time { return observed })
	must(t, err)
	if first == nil || first.ObservedAt != observed.Format(time.RFC3339Nano) {
		t.Fatalf("sealed record = %+v", first)
	}
	if validationErr := task.ValidateLifetimeUnlaunchedRecord(first); validationErr != nil {
		t.Fatalf("sealed record validation = %v", validationErr)
	}

	read, err := lifetime.ReadLifetimeUnlaunchedContext(t.Context())
	must(t, err)
	if !reflect.DeepEqual(read, first) {
		t.Fatalf("read fence = %+v, want %+v", read, first)
	}
	// A reversed clock cannot reopen or rewrite a committed fence. Replay uses
	// the durable timestamp rather than refreshing it from the new observation.
	replay, err := lifetime.sealLifetimeUnlaunchedContextWithNow(t.Context(), func() time.Time { return deadline.Add(-time.Hour) })
	must(t, err)
	if !reflect.DeepEqual(replay, first) {
		t.Fatalf("replayed fence = %+v, want immutable winner %+v", replay, first)
	}
}

func TestControlLifetimeUnlaunchedRejectsNativeStateAndUnreadableFence(t *testing.T) {
	t.Run("intent", func(t *testing.T) {
		deadline := time.Date(2026, time.February, 2, 3, 4, 5, 0, time.UTC)
		_, _, lifetime := newWorkspaceControlLifetime(t, deadline)
		must(t, recordWorkspaceLifetimeIntent(t, lifetime, deadline))
		if record, err := lifetime.sealLifetimeUnlaunchedContextWithNow(t.Context(), func() time.Time { return deadline.Add(time.Hour) }); record != nil || !errors.Is(err, task.ErrAlreadyStarted) {
			t.Fatalf("seal with intent = (%+v, %v), want nil/ErrAlreadyStarted", record, err)
		}
	})

	t.Run("registration", func(t *testing.T) {
		deadline := time.Date(2026, time.February, 3, 3, 4, 5, 0, time.UTC)
		_, _, lifetime := newWorkspaceControlLifetime(t, deadline)
		must(t, recordWorkspaceLifetimeIntent(t, lifetime, deadline))
		_, err := lifetime.RecordLifetimeRegistrationContext(t.Context(), LifetimeRegistrationFacts{
			ScopeKind: task.LifetimeScopeProcessGroup,
			Assurance: task.LifetimeAssuranceCooperative,
			ScopeID:   "workspace-scope",
			BootID:    "workspace-boot",
			Deadline:  deadline,
		})
		must(t, err)
		if record, err := lifetime.sealLifetimeUnlaunchedContextWithNow(t.Context(), func() time.Time { return deadline.Add(time.Hour) }); record != nil || !errors.Is(err, task.ErrAlreadyStarted) {
			t.Fatalf("seal with registration = (%+v, %v), want nil/ErrAlreadyStarted", record, err)
		}
	})

	t.Run("terminal", func(t *testing.T) {
		deadline := time.Date(2026, time.February, 4, 3, 4, 5, 0, time.UTC)
		_, _, lifetime := newWorkspaceControlLifetime(t, deadline)
		must(t, recordWorkspaceLifetimeIntent(t, lifetime, deadline))
		_, err := lifetime.RecordLifetimeRegistrationContext(t.Context(), LifetimeRegistrationFacts{
			ScopeKind: task.LifetimeScopeProcessGroup,
			Assurance: task.LifetimeAssuranceCooperative,
			ScopeID:   "workspace-scope",
			BootID:    "workspace-boot",
			Deadline:  deadline,
		})
		must(t, err)
		_, err = lifetime.RecordLifetimeTerminationContext(t.Context(), task.LifetimeTerminationBudget)
		must(t, err)
		err = lifetime.WithLifetimeLaunchPermissionContext(t.Context(), func(*ControlTransaction) error { return nil })
		if !errors.Is(err, task.ErrTerminalTask) {
			t.Fatalf("permission after termination = %v, want callback refusal", err)
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		deadline := time.Date(2026, time.February, 5, 3, 4, 5, 0, time.UTC)
		_, control, lifetime := newWorkspaceControlLifetime(t, deadline)
		_, err := control.Put(controlLifetimeUnlaunchedName, controlTestRecord{Request: "malformed-fence"})
		must(t, err)
		if record, err := lifetime.ReadLifetimeUnlaunchedContext(t.Context()); record != nil || task.RecordAbsent(err) || err == nil {
			t.Fatalf("malformed fence = (%+v, %v), want strict non-absence error", record, err)
		}
		if record, err := lifetime.sealLifetimeUnlaunchedContextWithNow(t.Context(), func() time.Time { return deadline.Add(time.Hour) }); record != nil || task.RecordAbsent(err) || err == nil {
			t.Fatalf("seal with malformed fence = (%+v, %v), want strict non-absence error", record, err)
		}
	})

	t.Run("conflicting-native-record", func(t *testing.T) {
		deadline := time.Date(2026, time.February, 6, 3, 4, 5, 0, time.UTC)
		_, control, lifetime := newWorkspaceControlLifetime(t, deadline)
		_, err := lifetime.sealLifetimeUnlaunchedContextWithNow(t.Context(), func() time.Time { return deadline.Add(time.Hour) })
		must(t, err)
		_, err = control.Put(controlLifetimeIntentName, task.LifetimeIntentRecord{
			SchemaVersion: task.SchemaVersion,
			RootID:        lifetime.rootID,
			TaskID:        lifetime.operationID,
			OwnerKind:     lifetime.ownerKind,
			OwnerSHA256:   lifetime.ownerSHA256,
			ScopeKind:     task.LifetimeScopeProcessGroup,
			Assurance:     task.LifetimeAssuranceCooperative,
			ScopePlan:     "contradictory-plan",
			BootID:        "contradictory-boot",
			Deadline:      lifetime.deadlineText(),
			CreatedAt:     deadline.Add(time.Hour).Format(time.RFC3339Nano),
		})
		must(t, err)
		if record, err := lifetime.ReadLifetimeUnlaunchedContext(t.Context()); record != nil || task.RecordAbsent(err) || err == nil {
			t.Fatalf("fence with contradictory intent = (%+v, %v), want strict non-absence error", record, err)
		}
	})
}

func TestControlLifetimeUnlaunchedCommitFaultReplaysMatchingWinner(t *testing.T) {
	deadline := time.Date(2026, time.March, 2, 3, 4, 5, 0, time.UTC)
	store, _, lifetime := newWorkspaceControlLifetime(t, deadline)
	injected := errors.New("injected post-link uncertainty")
	store.SetFaultInjector(&unlaunchedPostLinkFault{target: controlLifetimeUnlaunchedName, err: injected})
	observed := deadline.Add(time.Hour)
	first, err := lifetime.sealLifetimeUnlaunchedContextWithNow(t.Context(), func() time.Time { return observed })
	if first == nil || !errors.Is(err, task.ErrUncertainDurability) || !errors.Is(err, injected) {
		t.Fatalf("uncertain seal = (%+v, %v), want committed winner plus uncertainty", first, err)
	}
	store.SetFaultInjector(nil)

	read, err := lifetime.ReadLifetimeUnlaunchedContext(t.Context())
	must(t, err)
	if !reflect.DeepEqual(read, first) {
		t.Fatalf("winner after uncertain seal = %+v, want %+v", read, first)
	}
	replay, err := lifetime.sealLifetimeUnlaunchedContextWithNow(t.Context(), func() time.Time { return observed.Add(time.Hour) })
	must(t, err)
	if !reflect.DeepEqual(replay, first) {
		t.Fatalf("matching replay changed winner = %+v, want %+v", replay, first)
	}
}

func TestControlLifetimeUnlaunchedBusyOwnerAndPermissionCallback(t *testing.T) {
	deadline := time.Date(2026, time.April, 2, 3, 4, 5, 0, time.UTC)
	_, control, lifetime := newWorkspaceControlLifetime(t, deadline)
	owner, err := control.AcquireLifetimeOwner(t.Context())
	must(t, err)
	record, err := lifetime.sealLifetimeUnlaunchedContextWithNow(t.Context(), func() time.Time { return deadline.Add(time.Hour) })
	if record != nil || !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("seal with active owner = (%+v, %v), want ErrLockBusy", record, err)
	}
	must(t, owner.Release())

	_, err = lifetime.sealLifetimeUnlaunchedContextWithNow(t.Context(), func() time.Time { return deadline.Add(time.Hour) })
	must(t, err)
	called := false
	err = lifetime.WithLifetimeLaunchPermissionContext(t.Context(), func(*ControlTransaction) error {
		called = true
		return nil
	})
	if !errors.Is(err, task.ErrLifetimeUnlaunched) || called {
		t.Fatalf("callback after fence = %v, called=%t", err, called)
	}
}

func TestControlLifetimeUnlaunchedAndIntentRaceHaveOneWinner(t *testing.T) {
	deadline := time.Date(2026, time.May, 2, 3, 4, 5, 0, time.UTC)
	_, _, lifetime := newWorkspaceControlLifetime(t, deadline)
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	var fence *task.LifetimeUnlaunchedRecord
	var fenceErr error
	var intent *task.LifetimeIntentRecord
	var intentErr error
	go func() {
		defer wait.Done()
		<-start
		fence, fenceErr = lifetime.sealLifetimeUnlaunchedContextWithNow(t.Context(), func() time.Time { return deadline.Add(time.Hour) })
	}()
	go func() {
		defer wait.Done()
		<-start
		intent, intentErr = lifetime.RecordLifetimeIntentContext(t.Context(), LifetimeIntentFacts{
			ScopeKind: task.LifetimeScopeProcessGroup,
			Assurance: task.LifetimeAssuranceCooperative,
			ScopePlan: "workspace-plan",
			BootID:    "workspace-boot",
			Deadline:  deadline,
		})
	}()
	close(start)
	wait.Wait()

	if (fence == nil) == (intent == nil) {
		t.Fatalf("race winners fence=%+v err=%v intent=%+v err=%v; want exactly one durable winner", fence, fenceErr, intent, intentErr)
	}
	if fence != nil {
		if intentErr == nil || !errors.Is(intentErr, task.ErrLifetimeUnlaunched) {
			t.Fatalf("intent after fence = (%+v, %v), want ErrLifetimeUnlaunched", intent, intentErr)
		}
	} else if fenceErr == nil || !errors.Is(fenceErr, task.ErrAlreadyStarted) {
		t.Fatalf("fence after intent = (%+v, %v), want ErrAlreadyStarted", fence, fenceErr)
	}
}

func TestControlLifetimeRecordIntentRefusesFenceWithReversedClock(t *testing.T) {
	deadline := time.Date(2026, time.June, 2, 3, 4, 5, 0, time.UTC)
	_, _, lifetime := newWorkspaceControlLifetime(t, deadline)
	_, err := lifetime.sealLifetimeUnlaunchedContextWithNow(t.Context(), func() time.Time { return deadline.Add(time.Hour) })
	must(t, err)
	intent, err := lifetime.RecordLifetimeIntentContext(t.Context(), LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "reversed-clock-plan",
		BootID:    "reversed-clock-boot",
		Deadline:  deadline,
	})
	if intent != nil || !errors.Is(err, task.ErrLifetimeUnlaunched) {
		t.Fatalf("intent after reversed clock = (%+v, %v), want ErrLifetimeUnlaunched", intent, err)
	}
}

func newWorkspaceControlLifetime(t *testing.T, deadline time.Time) (*Store, *ControlDir, *ControlLifetime) {
	t.Helper()
	store := testStore(t)
	operationID := strings.Repeat("f", 32)
	control, err := store.OpenWorkspaceOperation(t.Context(), operationID, true)
	must(t, err)
	t.Cleanup(func() { must(t, control.Close()) })
	ownerSHA256 := task.ComputeSHA256([]byte("workspace-operation-owner"))
	lifetime, err := NewControlLifetime(control, store.RootID, operationID, task.LifetimeOwnerWorkspace, ownerSHA256, deadline)
	must(t, err)
	return store, control, lifetime
}

func recordWorkspaceLifetimeIntent(t *testing.T, lifetime *ControlLifetime, deadline time.Time) error {
	t.Helper()
	_, err := lifetime.RecordLifetimeIntentContext(t.Context(), LifetimeIntentFacts{
		ScopeKind: task.LifetimeScopeProcessGroup,
		Assurance: task.LifetimeAssuranceCooperative,
		ScopePlan: "workspace-plan",
		BootID:    "workspace-boot",
		Deadline:  deadline,
	})
	return err
}

type unlaunchedPostLinkFault struct {
	BaseFaultInjector
	target string
	err    error
}

func (f *unlaunchedPostLinkFault) OnPostLinkDirBarrier(path string) error {
	if filepath.Base(path) == f.target {
		return f.err
	}
	return nil
}
