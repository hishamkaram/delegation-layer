package infrastructure

import (
	"errors"
	"maps"
	"os/exec"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestWarmOwnerPublishedRootRequiresContactForEachContext(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "C1", true: "C2"}[changed], func(t *testing.T) {
			f := producerFixture(t)
			intent := seedProducerIntent(t, f, 1, false)
			seedWarmOwnerEvidence(t, f, intent, false)
			if changed {
				f.request.Context.ControlEnvironment = []string{"WARM_OWNER_CONTEXT=C2"}
			}
			before := producerBudgetTree(t, f.journal.pin.Root())
			starts := 0
			attempt, err := producerInvocation(t).establishOutside(t.Context(), f.journal.store, f.request, f.host, producerNative{start: func(*exec.Cmd) error { starts++; return errors.New("unexpected Start") }})
			ownProducerAttempt(t, attempt)
			var contact *OwnerContactRequiredError
			if !errors.As(err, &contact) || contact.Startup != settlementReference(t, intent) || attempt != nil || starts != 0 || f.host.calls != 0 {
				t.Fatalf("published owner failed contact selection: attempt=%t starts=%d prepares=%d err=%v", attempt != nil, starts, f.host.calls, err)
			}
			if !maps.Equal(before, producerBudgetTree(t, f.journal.pin.Root())) {
				t.Fatal("contact selection mutated immutable owner evidence")
			}
		})
	}
}

func TestWarmOwnerPendingContextsRemainStrictAndPreserveDeadline(t *testing.T) {
	for _, assumed := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "assumed-unpublished"}[assumed], func(t *testing.T) {
			exerciseWarmOwnerPendingContexts(t, assumed)
		})
	}
}

func exerciseWarmOwnerPendingContexts(t *testing.T, assumed bool) {
	t.Helper()
	f := producerFixture(t)
	intent := seedProducerIntent(t, f, 1, false)
	if assumed {
		assumption, _ := evidenceForIntent(t, intent)
		if created, err := f.journal.putAssumption(t.Context(), f.lease, assumption); err != nil || !created {
			t.Fatalf("assume pending fixture: %t %v", created, err)
		}
	}
	joined, err := producerInvocation(t).EstablishOutside(t.Context(), f.journal.store, f.request, f.host)
	ownProducerAttempt(t, joined)
	if err != nil || joined == nil || joined.intent != intent || joined.NativeCompletion() != nil {
		t.Fatalf("matching pending context changed birth/deadline: %v", err)
	}
	f.request.Context.ControlEnvironment = []string{"WARM_OWNER_CONTEXT=C2"}
	attempt, err := producerInvocation(t).EstablishOutside(t.Context(), f.journal.store, f.request, f.host)
	ownProducerAttempt(t, attempt)
	var contact *OwnerContactRequiredError
	if !errors.Is(err, task.ErrLockBusy) || errors.As(err, &contact) || errors.Is(err, ErrOwnerIncompatible) || attempt != nil || f.host.calls != 0 {
		t.Fatalf("different pending context joined or contacted: attempt=%t err=%v", attempt != nil, err)
	}
	stored, readErr := f.journal.readIntent(t.Context(), intent.Sequence)
	if readErr != nil || stored != intent {
		t.Fatalf("pending refusal changed original birth/deadline: %v", readErr)
	}
}

func TestWarmOwnerAbsentOwnerNeverContactsOrElects(t *testing.T) {
	for _, fault := range []string{"absent"} {
		t.Run(fault, func(t *testing.T) {
			f := producerFixture(t)
			intent := seedProducerIntent(t, f, 1, false)
			seedWarmOwnerEvidence(t, f, intent, fault == "absent")
			want := task.ErrLockBusy
			before := producerBudgetTree(t, f.journal.pin.Root())
			starts := 0
			attempt, err := producerInvocation(t).establishOutside(t.Context(), f.journal.store, f.request, f.host, producerNative{start: func(*exec.Cmd) error { starts++; return errors.New("unexpected Start") }})
			ownProducerAttempt(t, attempt)
			var contact *OwnerContactRequiredError
			if !errors.Is(err, want) || errors.As(err, &contact) || attempt != nil || starts != 0 || f.host.calls != 0 {
				t.Fatalf("unsafe %s result: attempt=%t starts=%d prepares=%d err=%v", fault, attempt != nil, starts, f.host.calls, err)
			}
			if !maps.Equal(before, producerBudgetTree(t, f.journal.pin.Root())) {
				t.Fatal("refusal changed durable startup or created settlement")
			}
		})
	}
}

func seedWarmOwnerEvidence(t *testing.T, f producerDataFixture, intent StartupIntent, absent bool) {
	t.Helper()
	assumption, publication := evidenceForIntent(t, intent)
	if absent {
		process := absentWarmOwnerProcess(t)
		assumption.Process = ProcessIdentity{PID: process.PID(), StartToken: process.StartToken()}
		var err error
		publication.AssumptionSHA256, err = assumption.Digest(intent)
		if err != nil {
			t.Fatal(err)
		}
		// Historical records name an exactly observed, already joined child.
		// Keeping the instance lease occupied must still prohibit election.
		if err = f.journal.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
			if _, putErr := tx.Put(assumptionName(intent.Sequence), assumption); putErr != nil {
				return putErr
			}
			_, putErr := tx.Put(publicationName(intent.Sequence), publication)
			return putErr
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	if created, err := f.journal.putAssumption(t.Context(), f.lease, assumption); err != nil || !created {
		t.Fatalf("publish live assumption: %t %v", created, err)
	}
	if created, err := f.journal.putPublication(t.Context(), f.lease, publication); err != nil || !created {
		t.Fatalf("publish live owner: %t %v", created, err)
	}
}

func TestWarmOwnerFreePendingContextMismatchRemainsBusy(t *testing.T) {
	f := producerFixture(t)
	intent := seedProducerIntent(t, f, 1, false)
	releaseProducerFixtureLease(t, f)
	f.request.Context.ControlEnvironment = []string{"WARM_OWNER_CONTEXT=C2"}
	before := producerBudgetTree(t, f.journal.pin.Root())
	starts := 0
	attempt, err := producerInvocation(t).establishOutside(t.Context(), f.journal.store, f.request, f.host, producerNative{start: func(*exec.Cmd) error { starts++; return errors.New("unexpected Start") }})
	ownProducerAttempt(t, attempt)
	var contact *OwnerContactRequiredError
	if !errors.Is(err, task.ErrLockBusy) || errors.As(err, &contact) || attempt != nil || f.host.calls != 0 || starts != 0 {
		t.Fatalf("free pending C1 accepted different C2: attempt=%t starts=%d prepares=%d err=%v", attempt != nil, starts, f.host.calls, err)
	}
	stored, readErr := f.journal.readIntent(t.Context(), intent.Sequence)
	if readErr != nil || stored != intent || !maps.Equal(before, producerBudgetTree(t, f.journal.pin.Root())) {
		t.Fatalf("free pending refusal renewed deadline or changed durable startup: %v", readErr)
	}
}

func absentWarmOwnerProcess(t *testing.T) lifetime.ProcessRef {
	t.Helper()
	executable, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable)
	waited := false
	t.Cleanup(func() {
		if !waited && command.Process != nil {
			if waitErr := command.Wait(); waitErr != nil {
				t.Error(waitErr)
			}
		}
	})
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	process, observeErr := lifetime.ObserveStartedProcess(command.Process.Pid)
	waitErr := command.Wait()
	waited = true
	if observeErr != nil || waitErr != nil {
		t.Fatalf("native incarnation observation/join: %v %v", observeErr, waitErr)
	}
	present, err := lifetime.ProcessPresent(process)
	if err != nil || present {
		t.Fatalf("exact joined child not absent: %t %v", present, err)
	}
	return process
}
