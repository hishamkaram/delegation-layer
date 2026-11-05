package infrastructure

import (
	"context"
	"errors"
	"maps"
	"os/exec"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func lifecycleUpgradeFixture(t *testing.T) (producerDataFixture, epochLifecycleIntent, StartupIntent) {
	t.Helper()
	fixture := producerFixture(t)
	body := journalLifecycleBody(t, fixture.journal, epochLifecycleOwnerUpgrade, 1)
	body.SuccessorArtifacts = fixture.host.artifacts
	requireLifecycleAccept(t, fixture.journal, fixture.lease, body)
	requireLifecycleProgress(t, fixture.journal, fixture.lease, body, epochLifecycleClosing)
	requireLifecycleProgress(t, fixture.journal, fixture.lease, body, epochLifecycleExiting)
	intent := lifecycleUpgradeStartup(t, fixture, body, 1, false)
	return fixture, body, intent
}

func completeLiveUpgrade(t *testing.T, fixture producerDataFixture, body epochLifecycleIntent, intent StartupIntent) {
	t.Helper()
	assumption, publication := evidenceForIntent(t, intent)
	publication.RuntimeSHA256 = body.SuccessorArtifacts.RunnerSHA256
	if created, err := fixture.journal.putAssumption(t.Context(), fixture.lease, assumption); err != nil || !created {
		t.Fatalf("upgrade self assumption: %t %v", created, err)
	}
	if created, err := fixture.journal.putPublication(t.Context(), fixture.lease, publication); err != nil || !created {
		t.Fatalf("upgrade runtime publication: %t %v", created, err)
	}
	progress := lifecycleProgress(t, body, epochLifecycleComplete)
	progress.Successor = settlementReference(t, intent)
	if created, err := fixture.journal.appendLifecycleProgress(t.Context(), fixture.lease, lifecycleReference(t, body), progress); err != nil || !created {
		t.Fatalf("own-EX upgrade completion: %t %v", created, err)
	}
}

func TestLifecycleLiveUpgradeRequiresAuthenticatedContact(t *testing.T) {
	fixture, body, intent := lifecycleUpgradeFixture(t)
	completeLiveUpgrade(t, fixture, body, intent)
	// Match the lifecycle's canonical selection; Upgrade startup fields are zero.
	fixture.request.Requirements = ArtifactRequirements{
		RequiredRuntimeSHA256: body.SuccessorArtifacts.RunnerSHA256,
		RequiredClientSHA256:  body.SuccessorArtifacts.ClientSHA256,
		RequiredDaemonSHA256:  body.SuccessorArtifacts.DaemonSHA256,
	}
	fixture.request.Context.ControlEnvironment = append(fixture.request.Context.ControlEnvironment, "NEW_SELECTION=context")
	attempt, err := EstablishOutside(t.Context(), fixture.journal.store, fixture.request, fixture.host)
	ownProducerAttempt(t, attempt)
	var contact *OwnerContactRequiredError
	if !errors.As(err, &contact) || contact.Startup != settlementReference(t, intent) || attempt != nil || fixture.host.calls != 0 {
		t.Fatalf("live Upgrade stranded or context-joined: attempt=%t prepare=%d err=%v", attempt != nil, fixture.host.calls, err)
	}
}

func TestLifecycleLiveUpgradeRejectsIncompatibleArtifactsBeforeContact(t *testing.T) {
	fixture, body, intent := lifecycleUpgradeFixture(t)
	completeLiveUpgrade(t, fixture, body, intent)
	fixture.request.Requirements.RequiredRuntimeSHA256 = task.ComputeSHA256([]byte("new trusted runtime selection"))
	fixture.request.Context.ControlEnvironment = append(fixture.request.Context.ControlEnvironment, "NEW_SELECTION=context")
	attempt, err := EstablishOutside(t.Context(), fixture.journal.store, fixture.request, fixture.host)
	ownProducerAttempt(t, attempt)
	var contact *OwnerContactRequiredError
	if !errors.Is(err, ErrIncompatible) || errors.As(err, &contact) || attempt != nil || fixture.host.calls != 0 {
		t.Fatalf("incompatible Upgrade contacted or prepared: attempt=%t prepare=%d err=%v", attempt != nil, fixture.host.calls, err)
	}
}

// The finite child provides an actually observed and exactly joined native
// incarnation. Canonical historical data below models prior-writer records;
// this is not managed-image or native resident-birth proof.
func absentLifecycleProcess(t *testing.T) lifetime.ProcessRef {
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

func completeDeadUpgradeHistory(t *testing.T, fixture producerDataFixture, body epochLifecycleIntent, intent StartupIntent) {
	t.Helper()
	progress := lifecycleProgress(t, body, epochLifecycleComplete)
	progress.Successor = settlementReference(t, intent)
	seedNativeAbsentLifecycleStartup(t, fixture, intent, body.SuccessorArtifacts.RunnerSHA256, &progress)
}

func seedNativeAbsentLifecycleStartup(t *testing.T, fixture producerDataFixture, intent StartupIntent, runtimeSHA string, progress *epochLifecycleProgress) {
	t.Helper()
	process := absentLifecycleProcess(t)
	assumption, publication := evidenceForIntent(t, intent)
	assumption.Process = ProcessIdentity{PID: process.PID(), StartToken: process.StartToken()}
	var err error
	publication.AssumptionSHA256, err = assumption.Digest(intent)
	if err != nil {
		t.Fatal(err)
	}
	publication.RuntimeSHA256 = runtimeSHA
	err = fixture.journal.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		if _, putErr := tx.Put(assumptionName(intent.Sequence), assumption); putErr != nil {
			return putErr
		}
		if _, putErr := tx.Put(publicationName(intent.Sequence), publication); putErr != nil {
			return putErr
		}
		if progress == nil {
			return nil
		}
		_, putErr := tx.Put(lifecycleProgressName(progress.OperationRef.ID, progress.Phase), *progress)
		return putErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, created, settleErr := fixture.journal.settleStartup(t.Context(), fixture.lease, settlementReference(t, intent)); settleErr != nil || !created {
		t.Fatalf("settle native-absent startup: %t %v", created, settleErr)
	}
}

func TestLifecycleCompletedDeadUpgradeAllowsOrdinaryBirthAndHistory(t *testing.T) {
	for _, scenario := range []string{"latest-upgrade", "upgrade-in-history", "later-aborted"} {
		t.Run(scenario, func(t *testing.T) { exerciseOrdinaryAfterUpgrade(t, scenario) })
	}
}

func exerciseOrdinaryAfterUpgrade(t *testing.T, scenario string) {
	t.Helper()
	fixture, body, intent := lifecycleUpgradeFixture(t)
	completeDeadUpgradeHistory(t, fixture, body, intent)
	expected := uint64(2)
	switch scenario {
	case "latest-upgrade":
	case "upgrade-in-history":
		second := seedProducerIntent(t, fixture, 2, true)
		if _, _, err := fixture.journal.settleStartup(t.Context(), fixture.lease, settlementReference(t, second)); err != nil {
			t.Fatal(err)
		}
		expected = 3
	case "later-aborted":
		later := journalLifecycleBody(t, fixture.journal, epochLifecycleOwnerUpgrade, 2)
		later.Owner = settlementReference(t, intent)
		requireLifecycleAccept(t, fixture.journal, fixture.lease, later)
		requireLifecycleProgress(t, fixture.journal, fixture.lease, later, epochLifecycleClosing)
		requireLifecycleProgress(t, fixture.journal, fixture.lease, later, epochLifecycleAborted)
	default:
		t.Fatal("unknown ordinary-Upgrade scenario")
	}
	releaseProducerFixtureLease(t, fixture)
	refused := errors.New("stop at native Start boundary")
	starts := 0
	attempt, err := establishOutside(t.Context(), fixture.journal.store, fixture.request, fixture.host, producerNative{start: func(*exec.Cmd) error { starts++; return refused }})
	ownProducerAttempt(t, attempt)
	if !errors.Is(err, refused) || attempt == nil || attempt.Reference().Sequence != expected || starts != 1 || fixture.host.calls != 1 {
		t.Fatalf("terminal Upgrade permanently fenced ordinary birth: starts=%d prepare=%d err=%v", starts, fixture.host.calls, err)
	}
}

func TestLifecycleUnresolvedUpgradeFencesOrdinaryBirthAndBusyJoin(t *testing.T) {
	for _, free := range []bool{false, true} {
		t.Run(map[bool]string{false: "busy-instance", true: "free-instance"}[free], func(t *testing.T) {
			fixture := producerFixture(t)
			intent := seedProducerIntent(t, fixture, 1, false)
			assumption, publication := evidenceForIntent(t, intent)
			if created, err := fixture.journal.putAssumption(t.Context(), fixture.lease, assumption); err != nil || !created {
				t.Fatal(err)
			}
			if created, err := fixture.journal.putPublication(t.Context(), fixture.lease, publication); err != nil || !created {
				t.Fatal(err)
			}
			body := journalLifecycleBody(t, fixture.journal, epochLifecycleOwnerUpgrade, 1)
			body.Owner = assumption.Startup
			requireLifecycleAccept(t, fixture.journal, fixture.lease, body)
			if free {
				releaseProducerFixtureLease(t, fixture)
			}
			attempt, err := EstablishOutside(t.Context(), fixture.journal.store, fixture.request, fixture.host)
			ownProducerAttempt(t, attempt)
			if !errors.Is(err, task.ErrLockBusy) || attempt != nil || fixture.host.calls != 0 {
				t.Fatalf("unresolved Upgrade escaped common lifecycle gate: attempt=%t err=%v", attempt != nil, err)
			}
		})
	}
}

func TestLifecycleTransactionFencesIntentAssumptionAndPublication(t *testing.T) {
	fixture := producerFixture(t)
	intent := seedProducerIntent(t, fixture, 1, false)
	assumption, publication := evidenceForIntent(t, intent)
	// Model a durable preexisting lifecycle intent through the canonical store;
	// current writer cannot elect it while this unassumed startup is unresolved.
	body := journalLifecycleBody(t, fixture.journal, epochLifecycleStop, 1)
	body.Owner = assumption.Startup
	if err := fixture.journal.control.WithLock(t.Context(), func(tx *taskdir.ControlTransaction) error {
		_, putErr := tx.Put(lifecycleName(body.ID), body)
		return putErr
	}); err != nil {
		t.Fatal(err)
	}
	if created, err := fixture.journal.putAssumption(t.Context(), fixture.lease, assumption); created || !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("assumption bypass: %t %v", created, err)
	}
	if created, err := fixture.journal.putPublication(t.Context(), fixture.lease, publication); created || !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("publication bypass: %t %v", created, err)
	}
	intent.Sequence++
	intent.Epoch = task.ComputeSHA256([]byte("new lifecycle-fenced epoch"))[:32]
	refreshIntentDigest(t, &intent)
	if created, err := fixture.journal.putIntent(t.Context(), fixture.lease, intent); created || !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("intent bypass: %t %v", created, err)
	}
}

func TestLifecycleResidentStartAndBindingCommitShareFence(t *testing.T) {
	fixture := newOwnerServerFixture(t, &ownerServerTestHost{})
	entry := fixture.entry
	root := entry.operation.Intent().Inputs.Root
	calls := 0
	callback := func() error { calls++; return nil }
	if err := entry.grant.GuardDaemonStart(t.Context(), root, callback); err != nil {
		t.Fatal(err)
	}
	if err := entry.grant.GuardPrivateGenerationCommit(t.Context(), root, callback); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("positive native callback controls: %d", calls)
	}
	body := journalLifecycleBody(t, entry.journal, epochLifecycleStop, 1)
	body.Owner = settlementReference(t, entry.operation.Intent())
	requireLifecycleAccept(t, entry.journal, entry.lease, body)
	if err := entry.grant.GuardDaemonStart(t.Context(), root, callback); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("Start bypassed accepted lifecycle: %v", err)
	}
	if err := entry.grant.GuardPrivateGenerationCommit(t.Context(), root, callback); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("binding commit bypassed accepted lifecycle: %v", err)
	}
	if calls != 2 {
		t.Fatalf("lifecycle-fenced callback ran: %d", calls)
	}
}

func TestLifecycleWarmTaskAdmissionDiffersFromBirthIncludingAbandoned(t *testing.T) {
	for _, kind := range []epochLifecycleKind{epochLifecycleStop, epochLifecycleOwnerUpgrade} {
		t.Run(string(kind), func(t *testing.T) { checkWarmLifecycleAdmission(t, kind) })
	}
}

func checkWarmLifecycleAdmission(t *testing.T, kind epochLifecycleKind) {
	t.Helper()
	j, lease := journalFixture(t)
	body := journalLifecycleBody(t, j, kind, 1)
	requireLifecycleAccept(t, j, lease, body)
	reader, err := taskdir.OpenReadOnlyStoreContext(t.Context(), j.pin.Root(), predicate.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	phases := []epochLifecyclePhase{epochLifecycleAccepted, epochLifecycleClosing}
	if kind == epochLifecycleOwnerUpgrade {
		phases = append(phases, epochLifecycleExiting, epochLifecycleAbandoned)
	} else {
		phases = append(phases, epochLifecycleStopping)
	}
	for _, phase := range phases {
		if phase != epochLifecycleAccepted {
			requireLifecycleProgress(t, j, lease, body, phase)
		}
		requireWarmLifecycleDecision(t, j, reader, kind, phase)
	}
}

func requireWarmLifecycleDecision(t *testing.T, j *intentJournal, reader *taskdir.ReadOnlyStore, kind epochLifecycleKind, phase epochLifecyclePhase) {
	t.Helper()
	before := producerBudgetTree(t, j.pin.Root())
	admissionErr := CheckPrivateTaskAdmissionContext(t.Context(), reader)
	if kind == epochLifecycleStop && !errors.Is(admissionErr, task.ErrLockBusy) {
		t.Fatalf("pending Stop admitted warm task: phase=%s err=%v", phase, admissionErr)
	}
	if kind == epochLifecycleOwnerUpgrade && admissionErr != nil {
		t.Fatalf("owner-only Upgrade closed warm task admission: phase=%s err=%v", phase, admissionErr)
	}
	snapshot, found, err := j.latestLifecycle(t.Context())
	if err != nil || !found {
		t.Fatal(err)
	}
	if err = checkLifecycleAction(snapshot, found, lifecycleOrdinaryBirth); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("warm exception opened ordinary birth: phase=%s err=%v", phase, err)
	}
	if err = checkLifecycleAction(snapshot, found, lifecycleGenerationMutation); !errors.Is(err, task.ErrLockBusy) {
		t.Fatalf("warm exception opened generation mutation: phase=%s err=%v", phase, err)
	}
	if !maps.Equal(before, producerBudgetTree(t, j.pin.Root())) {
		t.Fatal("passive admission wrote journal state")
	}
}

func TestLifecycleGenerationCommitPreservesExactResultAfterCancellation(t *testing.T) {
	fixture := newOwnerServerFixture(t, &ownerServerTestHost{})
	expected := &componentStartError{message: "exact binding commit result"}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	result := fixture.entry.grant.GuardPrivateGenerationCommit(ctx, fixture.root, func() error { calls++; cancel(); return expected })
	requireExactComponentStartResult(t, result, expected)
	if calls != 1 {
		t.Fatalf("binding commit callback count=%d", calls)
	}
}

func lifecycleUpgradeStartup(t *testing.T, fixture producerDataFixture, body epochLifecycleIntent, sequence uint64, expired bool) StartupIntent {
	t.Helper()
	intent := liveJournalIntent(t, fixture.journal)
	intent.Sequence = sequence
	intent.Epoch = task.ComputeSHA256([]byte(body.ID + string(rune(sequence))))[:32]
	intent.Inputs.Kind = StartupUpgrade
	intent.Inputs.Artifacts, intent.Inputs.Context = Artifacts{}, LaunchContextReference{}
	intent.Inputs.UpgradeID, intent.Inputs.UpgradeSHA256 = body.ID, lifecycleReference(t, body).SHA256
	if expired {
		intent.AcceptedAt = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
		intent.Deadline = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	}
	refreshIntentDigest(t, &intent)
	requireJournalPut(t, fixture.journal, fixture.lease, intent)
	return intent
}

func TestLifecycleAbandonedRequiresOriginalOwnerAndAllLaterSettlements(t *testing.T) {
	fixture, completed, original := lifecycleUpgradeFixture(t)
	completeDeadUpgradeHistory(t, fixture, completed, original)
	body := journalLifecycleBody(t, fixture.journal, epochLifecycleOwnerUpgrade, 2)
	body.Owner = settlementReference(t, original)
	requireLifecycleAccept(t, fixture.journal, fixture.lease, body)
	requireLifecycleProgress(t, fixture.journal, fixture.lease, body, epochLifecycleClosing)
	requireLifecycleProgress(t, fixture.journal, fixture.lease, body, epochLifecycleExiting)
	failed := lifecycleUpgradeStartup(t, fixture, body, 2, true)
	abandoned := lifecycleProgress(t, body, epochLifecycleAbandoned)
	if created, err := fixture.journal.appendLifecycleProgress(t.Context(), fixture.lease, lifecycleReference(t, body), abandoned); created || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unsettled later attempt permitted abandonment: %t %v", created, err)
	}
	if _, created, err := fixture.journal.settleStartup(t.Context(), fixture.lease, settlementReference(t, failed)); err != nil || !created {
		t.Fatalf("settle failed successor: %t %v", created, err)
	}
	requireLifecycleProgress(t, fixture.journal, fixture.lease, body, epochLifecycleAbandoned)
	attempt, err := EstablishOutside(t.Context(), fixture.journal.store, fixture.request, fixture.host)
	ownProducerAttempt(t, attempt)
	if !errors.Is(err, task.ErrLockBusy) || attempt != nil || fixture.host.calls != 0 {
		t.Fatalf("ordinary birth reopened abandoned Upgrade: %v", err)
	}
	next := journalLifecycleBody(t, fixture.journal, epochLifecycleStop, 3)
	next.Owner = settlementReference(t, failed)
	if created, acceptErr := fixture.journal.acceptLifecycle(t.Context(), fixture.lease, next); created || !errors.Is(acceptErr, ErrIdentity) {
		t.Fatalf("new body retargeted abandoned logical owner: %t %v", created, acceptErr)
	}
	next.Owner = body.Owner
	requireLifecycleAccept(t, fixture.journal, fixture.lease, next)
	if created, continueErr := fixture.journal.appendLifecycleProgress(t.Context(), fixture.lease, lifecycleReference(t, body), abandoned); created || !errors.Is(continueErr, ErrIdentity) {
		t.Fatalf("superseded body continuation: %t %v", created, continueErr)
	}
}

// Reconciliation carries the original logical target without selecting the
// failed published successor. Zero never substitutes for native closure proof.
func TestLifecycleInheritedZeroOwnerAfterPublishedSuccessor(t *testing.T) {
	fixture, body, intent := lifecycleUpgradeFixture(t)
	if body.Owner != (StartupReference{}) {
		t.Fatal("fixture must have an explicit original no-owner body")
	}
	seedNativeAbsentLifecycleStartup(t, fixture, intent, body.SuccessorArtifacts.RunnerSHA256, nil)
	if _, err := fixture.journal.readPublication(t.Context(), settlementReference(t, intent)); err != nil {
		t.Fatalf("successor must have published before exit: %v", err)
	}
	snapshot, found, err := fixture.journal.latestLifecycle(t.Context())
	if err != nil || !found || snapshot.phase != epochLifecycleExiting {
		t.Fatalf("successor must not have completed Upgrade: %v", err)
	}
	requireLifecycleProgress(t, fixture.journal, fixture.lease, body, epochLifecycleAbandoned)
	next := journalLifecycleBody(t, fixture.journal, epochLifecycleStop, 2)
	next.Owner = body.Owner
	requireLifecycleAccept(t, fixture.journal, fixture.lease, next)
	latest, found, err := fixture.journal.latestLifecycle(t.Context())
	if err != nil || !found || latest.intent.Owner != (StartupReference{}) {
		t.Fatalf("original-zero reconciliation semantics changed: %v", err)
	}
	if created, continueErr := fixture.journal.appendLifecycleProgress(t.Context(), fixture.lease, lifecycleReference(t, body), lifecycleProgress(t, body, epochLifecycleAbandoned)); created || !errors.Is(continueErr, ErrIdentity) {
		t.Fatalf("old zero-owner body continuation escaped fence: %t %v", created, continueErr)
	}
}
