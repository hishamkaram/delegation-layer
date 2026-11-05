package infrastructure

import (
	"errors"
	"maps"
	"os/exec"
	"testing"

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
			attempt, err := establishOutside(t.Context(), f.journal.store, f.request, f.host, producerNative{start: func(*exec.Cmd) error { starts++; return errors.New("unexpected Start") }})
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
	joined, err := EstablishOutside(t.Context(), f.journal.store, f.request, f.host)
	ownProducerAttempt(t, joined)
	if err != nil || joined == nil || joined.intent != intent || joined.NativeCompletion() != nil {
		t.Fatalf("matching pending context changed birth/deadline: %v", err)
	}
	f.request.Context.ControlEnvironment = []string{"WARM_OWNER_CONTEXT=C2"}
	attempt, err := EstablishOutside(t.Context(), f.journal.store, f.request, f.host)
	ownProducerAttempt(t, attempt)
	var contact *OwnerContactRequiredError
	if !errors.Is(err, task.ErrLockBusy) || errors.As(err, &contact) || errors.Is(err, ErrIncompatible) || attempt != nil || f.host.calls != 0 {
		t.Fatalf("different pending context joined or contacted: attempt=%t err=%v", attempt != nil, err)
	}
	stored, readErr := f.journal.readIntent(t.Context(), intent.Sequence)
	if readErr != nil || stored != intent {
		t.Fatalf("pending refusal changed original birth/deadline: %v", readErr)
	}
}

func TestWarmOwnerArtifactMismatchAndAbsentOwnerNeverContactOrElect(t *testing.T) {
	for _, fault := range []string{"artifact", "absent"} {
		t.Run(fault, func(t *testing.T) {
			f := producerFixture(t)
			intent := seedProducerIntent(t, f, 1, false)
			seedWarmOwnerEvidence(t, f, intent, fault == "absent")
			want := task.ErrLockBusy
			if fault == "artifact" {
				f.request.Requirements.RequiredDaemonSHA256 = task.ComputeSHA256([]byte("other daemon"))
				want = ErrIncompatible
			}
			before := producerBudgetTree(t, f.journal.pin.Root())
			starts := 0
			attempt, err := establishOutside(t.Context(), f.journal.store, f.request, f.host, producerNative{start: func(*exec.Cmd) error { starts++; return errors.New("unexpected Start") }})
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
		process := absentLifecycleProcess(t)
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

func TestWarmOwnerExitingLifecycleBlocksContact(t *testing.T) {
	f := producerFixture(t)
	intent := seedProducerIntent(t, f, 1, false)
	seedWarmOwnerEvidence(t, f, intent, false)
	body := journalLifecycleBody(t, f.journal, epochLifecycleOwnerUpgrade, 1)
	body.Owner = settlementReference(t, intent)
	requireLifecycleAccept(t, f.journal, f.lease, body)
	requireLifecycleProgress(t, f.journal, f.lease, body, epochLifecycleClosing)
	requireLifecycleProgress(t, f.journal, f.lease, body, epochLifecycleExiting)
	f.request.Context.ControlEnvironment = []string{"WARM_OWNER_CONTEXT=C2"}
	before := producerBudgetTree(t, f.journal.pin.Root())
	attempt, err := EstablishOutside(t.Context(), f.journal.store, f.request, f.host)
	ownProducerAttempt(t, attempt)
	var contact *OwnerContactRequiredError
	if !errors.Is(err, task.ErrLockBusy) || errors.As(err, &contact) || attempt != nil || f.host.calls != 0 {
		t.Fatalf("Exiting lifecycle allowed contact or election: attempt=%t err=%v", attempt != nil, err)
	}
	if !maps.Equal(before, producerBudgetTree(t, f.journal.pin.Root())) {
		t.Fatal("Exiting refusal mutated lifecycle/startup evidence")
	}
}

func TestWarmOwnerUpgradeChecksCanonicalSuccessorArtifacts(t *testing.T) {
	for _, mismatch := range []string{"none", "runtime", "client", "daemon"} {
		t.Run(mismatch, func(t *testing.T) {
			f, body, intent := lifecycleUpgradeFixture(t)
			completeLiveUpgrade(t, f, body, intent)
			f.request.Context.ControlEnvironment = []string{"WARM_OWNER_CONTEXT=C2"}
			different := task.ComputeSHA256([]byte("different accepted artifact"))
			switch mismatch {
			case "none":
			case "runtime":
				f.request.Requirements.RequiredRuntimeSHA256 = different
			case "client":
				f.request.Requirements.RequiredClientSHA256 = different
			case "daemon":
				f.request.Requirements.RequiredDaemonSHA256 = different
			}
			before := producerBudgetTree(t, f.journal.pin.Root())
			attempt, err := EstablishOutside(t.Context(), f.journal.store, f.request, f.host)
			ownProducerAttempt(t, attempt)
			var contact *OwnerContactRequiredError
			contacted := errors.As(err, &contact)
			if mismatch == "none" {
				if !contacted || contact.Startup != settlementReference(t, intent) {
					t.Fatalf("canonical successor artifacts not used: %v", err)
				}
			} else if contacted || !errors.Is(err, ErrIncompatible) {
				t.Fatalf("Upgrade dropped %s artifact requirement: %v", mismatch, err)
			}
			if attempt != nil || f.host.calls != 0 || !maps.Equal(before, producerBudgetTree(t, f.journal.pin.Root())) {
				t.Fatal("Upgrade contact/refusal prepared or changed stored lifecycle/startup")
			}
		})
	}
}

func TestWarmOwnerFreePendingContextMismatchRemainsBusy(t *testing.T) {
	f := producerFixture(t)
	intent := seedProducerIntent(t, f, 1, false)
	releaseProducerFixtureLease(t, f)
	f.request.Context.ControlEnvironment = []string{"WARM_OWNER_CONTEXT=C2"}
	before := producerBudgetTree(t, f.journal.pin.Root())
	starts := 0
	attempt, err := establishOutside(t.Context(), f.journal.store, f.request, f.host, producerNative{start: func(*exec.Cmd) error { starts++; return errors.New("unexpected Start") }})
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

func TestWarmOwnerUpgradeRejectsPublishedRuntimeOutsideSuccessorSelection(t *testing.T) {
	f := producerFixture(t)
	seedWarmHistoricalAbortedUpgrade(t, f)
	before := producerBudgetTree(t, f.journal.pin.Root())
	attempt, err := EstablishOutside(t.Context(), f.journal.store, f.request, f.host)
	ownProducerAttempt(t, attempt)
	var contact *OwnerContactRequiredError
	if !errors.Is(err, ErrIdentity) || errors.Is(err, task.ErrEvidenceFault) || errors.As(err, &contact) || attempt != nil || f.host.calls != 0 {
		t.Fatalf("Upgrade publication bypassed canonical runtime selection: attempt=%t prepare=%d err=%v", attempt != nil, f.host.calls, err)
	}
	if !maps.Equal(before, producerBudgetTree(t, f.journal.pin.Root())) {
		t.Fatal("runtime rejection changed stored Upgrade evidence")
	}
}

// Historical records deliberately leave an unsettled Upgrade beside an aborted
// lifecycle. The ordinary history gate accepts this shape; the contact boundary
// must independently reject its published runtime. No child is launched here.
func seedWarmHistoricalAbortedUpgrade(t *testing.T, f producerDataFixture) {
	t.Helper()
	body := journalLifecycleBody(t, f.journal, epochLifecycleOwnerUpgrade, 1)
	body.SuccessorArtifacts = f.host.artifacts
	requireLifecycleAccept(t, f.journal, f.lease, body)
	requireLifecycleProgress(t, f.journal, f.lease, body, epochLifecycleClosing)
	intent := liveJournalIntent(t, f.journal)
	intent.Inputs.Kind = StartupUpgrade
	intent.Inputs.Artifacts, intent.Inputs.Context = Artifacts{}, LaunchContextReference{}
	intent.Inputs.UpgradeID, intent.Inputs.UpgradeSHA256 = body.ID, lifecycleReference(t, body).SHA256
	refreshIntentDigest(t, &intent)
	assumption, publication := evidenceForIntent(t, intent)
	publication.RuntimeSHA256 = task.ComputeSHA256([]byte("runtime outside canonical successor artifacts"))
	if err := publication.Validate(intent, assumption); err != nil {
		t.Fatalf("negative fixture must be structurally valid Upgrade publication: %v", err)
	}
	progress := lifecycleProgress(t, body, epochLifecycleAborted)
	if err := f.journal.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		if _, putErr := tx.Put(intentName(intent.Sequence), intent); putErr != nil {
			return putErr
		}
		if _, putErr := tx.Put(assumptionName(intent.Sequence), assumption); putErr != nil {
			return putErr
		}
		if _, putErr := tx.Put(publicationName(intent.Sequence), publication); putErr != nil {
			return putErr
		}
		_, putErr := tx.Put(lifecycleProgressName(body.ID, epochLifecycleAborted), progress)
		return putErr
	}); err != nil {
		t.Fatal(err)
	}
}
