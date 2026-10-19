package taskdir

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	handoffProvider     = "fixture:test"
	handoffConversation = "handoff-session"
	handoffCurrentBoot  = "handoff-current-boot"
)

// recordHandoffLifetime writes one canonical closure kind through the
// production lifetime writers. Open writes the registered worker only.
func recordHandoffLifetime(t *testing.T, td *TaskDir, kind task.LifetimeClosureKind) {
	t.Helper()
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	defer func() { must(t, owner.Release()) }()
	facts := prepareCompletionTestIntent(t, td)
	if kind == task.LifetimeClosureUnregisteredRecovered {
		_, err = td.RecordLifetimeWorkerAbsenceContext(t.Context(), task.LifetimeAbsenceUnregisteredDisposed, facts.BootID)
		must(t, err)
		return
	}
	_, err = td.RecordLifetimeRegistrationContext(t.Context(), facts)
	must(t, err)
	worker, err := td.RecordLifetimeWorkerContext(t.Context(), LifetimeWorkerFacts{PID: 4242, ProcessStart: "handoff-start", BootID: facts.BootID})
	must(t, err)
	switch kind {
	case task.LifetimeClosureOpen:
	case task.LifetimeClosureRegisteredNative:
		workerData, marshalErr := task.MarshalCanonical(worker)
		must(t, marshalErr)
		exitCode := 1
		_, err = td.RecordLifetimeWorkerWaitContext(t.Context(), LifetimeWorkerWaitFacts{WorkerSHA256: task.ComputeSHA256(workerData), ChildPID: worker.PID, Result: task.LifetimeWorkerWaitExitError, ExitCode: &exitCode})
		must(t, err)
		_, err = td.RecordLifetimeTerminationContext(t.Context(), task.LifetimeTerminationCompleted)
		must(t, err)
	case task.LifetimeClosureLegacyPreviousBoot:
		_, err = td.RecordLifetimeTerminationContext(t.Context(), task.LifetimeTerminationPreviousBoot)
		must(t, err)
	case task.LifetimeClosureRegisteredRecovered:
		_, err = td.RecordLifetimeTerminationContext(t.Context(), task.LifetimeTerminationPreviousBoot)
		must(t, err)
		_, err = td.RecordLifetimeWorkerAbsenceContext(t.Context(), task.LifetimeAbsencePreviousBoot, handoffCurrentBoot)
		must(t, err)
	case task.LifetimeClosureUnregisteredRecovered:
		t.Fatal("unregistered closure has no registration")
	}
}

func recordHandoffPreviousBootAbsence(t *testing.T, td *TaskDir) {
	t.Helper()
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	defer func() { must(t, owner.Release()) }()
	_, err = td.RecordLifetimeWorkerAbsenceContext(t.Context(), task.LifetimeAbsencePreviousBoot, handoffCurrentBoot)
	must(t, err)
}

// requireHandoffClosure proves the fixture produced the named canonical kind.
func requireHandoffClosure(t *testing.T, td *TaskDir, want task.LifetimeClosureKind) task.LifetimeEvidence {
	t.Helper()
	evidence, err := LoadLifetimeEvidenceContext(t.Context(), td)
	must(t, err)
	closure, err := task.ResolveLifetimeClosure(evidence)
	if err != nil || closure.Kind != want {
		t.Fatalf("closure = %+v err=%v, want %s", closure, err, want)
	}
	return evidence
}

func handoffTask(t *testing.T, s *Store) *TaskDir {
	t.Helper()
	td := supervisorTask(t, s)
	must(t, td.ClaimSession(handoffProvider, handoffConversation))
	return td
}

func handoffReleasePath(td *TaskDir) string {
	return filepath.Join(td.sessionDirectory(handoffProvider, handoffConversation), td.TaskID+".release.json")
}

func readHandoffRelease(t *testing.T, td *TaskDir) (task.SessionReleaseRecord, []byte) {
	t.Helper()
	data := readTestFile(t, handoffReleasePath(td))
	var release task.SessionReleaseRecord
	must(t, task.DecodeStrict(data, &release))
	return release, data
}

func requireNoHandoffPublication(t *testing.T, td *TaskDir) {
	t.Helper()
	testAbsent(t, filepath.Join(td.Dir, "outcome.json"))
	testAbsent(t, filepath.Join(td.Dir, "provider.exit"))
}

func requireHandoffSuccessor(t *testing.T, s *Store) {
	t.Helper()
	successor := preparedTask(t, s)
	must(t, successor.ClaimSession(handoffProvider, handoffConversation))
}

func requireHandoffSuccessorRefused(t *testing.T, s *Store) {
	t.Helper()
	successor := preparedTask(t, s)
	if err := successor.ClaimSession(handoffProvider, handoffConversation); err == nil {
		t.Fatal("successor claimed a session without a valid predecessor handoff")
	}
}

func canonicalTestDigest[T handoffWitnessRecord](t *testing.T, record T) string {
	t.Helper()
	digest, err := canonicalRecordDigest(record)
	must(t, err)
	return digest
}

func TestLifetimeClosureReleaseAcceptsEveryClosedKind(t *testing.T) {
	for _, kind := range []task.LifetimeClosureKind{task.LifetimeClosureRegisteredNative, task.LifetimeClosureRegisteredRecovered, task.LifetimeClosureLegacyPreviousBoot, task.LifetimeClosureUnregisteredRecovered} {
		t.Run(string(kind), func(t *testing.T) {
			s := testStore(t)
			td := handoffTask(t, s)
			recordHandoffLifetime(t, td, kind)
			evidence := requireHandoffClosure(t, td, kind)
			must(t, td.ReleaseSessionAfterLifetimeClosure(handoffProvider, handoffConversation))

			release, _ := readHandoffRelease(t, td)
			want := ""
			if evidence.Termination != nil {
				want = canonicalTestDigest(t, evidence.Termination)
			} else {
				want = canonicalTestDigest(t, evidence.WorkerWait)
			}
			if release.PredecessorEvidenceSHA256 != want {
				t.Fatalf("release digest = %s, want preferred closure witness %s", release.PredecessorEvidenceSHA256, want)
			}
			requireNoHandoffPublication(t, td)
			requireHandoffSuccessor(t, s)
		})
	}
}

func TestLifetimeClosureReleaseRefusesOpenLifetimeAndHeldRunLease(t *testing.T) {
	t.Run("open", func(t *testing.T) {
		s := testStore(t)
		td := handoffTask(t, s)
		recordHandoffLifetime(t, td, task.LifetimeClosureOpen)
		if err := td.ReleaseSessionAfterLifetimeClosure(handoffProvider, handoffConversation); !errors.Is(err, task.ErrSessionBusy) {
			t.Fatalf("open lifetime release = %v, want %v", err, task.ErrSessionBusy)
		}
		testAbsent(t, handoffReleasePath(td))
		requireHandoffSuccessorRefused(t, s)
	})
	t.Run("held run lease", func(t *testing.T) {
		s := testStore(t)
		td := handoffTask(t, s)
		start := consumeStart(t, td)
		defer func() { must(t, start.Release()) }()
		recordHandoffLifetime(t, td, task.LifetimeClosureRegisteredRecovered)
		if err := td.ReleaseSessionAfterLifetimeClosure(handoffProvider, handoffConversation); !errors.Is(err, task.ErrSessionBusy) {
			t.Fatalf("held run lease release = %v, want %v", err, task.ErrSessionBusy)
		}
		testAbsent(t, handoffReleasePath(td))
		requireHandoffSuccessorRefused(t, s)
	})
}

func TestLifetimeClosureReleaseRefusesUnknownProofAndCompetingOwner(t *testing.T) {
	s := testStore(t)
	td := handoffTask(t, s)
	recordHandoffLifetime(t, td, task.LifetimeClosureRegisteredRecovered)
	unknown := task.ComputeSHA256([]byte("not a handoff witness"))
	if err := td.ReleaseSession(handoffProvider, handoffConversation, unknown); err == nil {
		t.Fatal("an unknown digest released the session")
	}
	testAbsent(t, handoffReleasePath(td))

	must(t, td.ReleaseSessionAfterLifetimeClosure(handoffProvider, handoffConversation))
	successor := preparedTask(t, s)
	must(t, successor.ClaimSession(handoffProvider, handoffConversation))
	competitor := preparedTask(t, s)
	if err := competitor.ClaimSession(handoffProvider, handoffConversation); !errors.Is(err, task.ErrSessionBusy) {
		t.Fatalf("competing claim = %v, want %v", err, task.ErrSessionBusy)
	}
	recordHandoffLifetime(t, competitor, task.LifetimeClosureRegisteredRecovered)
	if err := competitor.ReleaseSessionAfterLifetimeClosure(handoffProvider, handoffConversation); err == nil {
		t.Fatal("a task without the claim released the active owner's session")
	}
}

func TestLifetimeClosureReleaseRefusesMalformedRequiredClosure(t *testing.T) {
	s := testStore(t)
	td := handoffTask(t, s)
	recordHandoffLifetime(t, td, task.LifetimeClosureRegisteredRecovered)
	writeTestFile(t, filepath.Join(td.Dir, lifetimeDirectory, lifetimeTerminationName), []byte("{"))
	if err := td.ReleaseSessionAfterLifetimeClosure(handoffProvider, handoffConversation); err == nil {
		t.Fatal("malformed closure evidence released the session")
	}
	testAbsent(t, handoffReleasePath(td))
	requireHandoffSuccessorRefused(t, s)
}

func TestLifetimeClosureReleaseSurvivesLaterBudgetObservation(t *testing.T) {
	s := testStore(t)
	td := handoffTask(t, s)
	supervisorSubmit(t, td)
	supervisorReceipt(t, td)
	must(t, consumeStart(t, td).Release())
	recordHandoffLifetime(t, td, task.LifetimeClosureRegisteredRecovered)
	must(t, td.ReleaseSessionAfterLifetimeClosure(handoffProvider, handoffConversation))
	_, before := readHandoffRelease(t, td)

	recordHandoffBudgetStop(t, td)
	must(t, td.ReleaseSessionAfterTimeout(handoffProvider, handoffConversation))
	requireHandoffReleaseBytes(t, td, before)
	requireHandoffSuccessor(t, s)
}

func TestLegacyPreviousBootReleaseSurvivesLaterMatchingAbsence(t *testing.T) {
	s := testStore(t)
	td := handoffTask(t, s)
	recordHandoffLifetime(t, td, task.LifetimeClosureLegacyPreviousBoot)
	must(t, td.ReleaseSessionAfterLifetimeClosure(handoffProvider, handoffConversation))
	_, before := readHandoffRelease(t, td)

	recordHandoffPreviousBootAbsence(t, td)
	requireHandoffClosure(t, td, task.LifetimeClosureRegisteredRecovered)
	must(t, td.ReleaseSessionAfterLifetimeClosure(handoffProvider, handoffConversation))
	requireHandoffReleaseBytes(t, td, before)
	requireHandoffSuccessor(t, s)
}

func TestLifetimeClosureReleaseSurvivesLateOutcome(t *testing.T) {
	s := testStore(t)
	td := handoffTask(t, s)
	sealHandoffTask(t, td)
	recordHandoffLifetime(t, td, task.LifetimeClosureRegisteredNative)
	must(t, td.ReleaseSessionAfterLifetimeClosure(handoffProvider, handoffConversation))
	_, before := readHandoffRelease(t, td)

	outcome := publishHandoffOutcome(t, td)
	must(t, td.ReleaseSession(handoffProvider, handoffConversation, outcome.EvidenceSHA256))
	requireHandoffReleaseBytes(t, td, before)
	requireHandoffSuccessor(t, s)
}

// Optional lifetime evidence is not part of an outcome or budget proof, so its
// corruption cannot revoke an earlier release proven by either.
func TestMalformedLifetimeCannotRevokeOutcomeOrBudgetRelease(t *testing.T) {
	t.Run("outcome", func(t *testing.T) {
		s := testStore(t)
		td := handoffTask(t, s)
		sealHandoffTask(t, td)
		outcome := publishHandoffOutcome(t, td)
		must(t, td.ReleaseSession(handoffProvider, handoffConversation, outcome.EvidenceSHA256))
		corruptHandoffLifetime(t, td)
		requireHandoffSuccessor(t, s)
	})
	t.Run("budget", func(t *testing.T) {
		s := testStore(t)
		td := handoffTask(t, s)
		supervisorSubmit(t, td)
		supervisorReceipt(t, td)
		must(t, consumeStart(t, td).Release())
		recordHandoffBudgetStop(t, td)
		must(t, td.ReleaseSessionAfterTimeout(handoffProvider, handoffConversation))
		corruptHandoffLifetime(t, td)
		requireHandoffSuccessor(t, s)
	})
}

func TestCorruptClosureRevokesOnlyAClosureRelease(t *testing.T) {
	s := testStore(t)
	td := handoffTask(t, s)
	recordHandoffLifetime(t, td, task.LifetimeClosureRegisteredRecovered)
	must(t, td.ReleaseSessionAfterLifetimeClosure(handoffProvider, handoffConversation))
	corruptHandoffLifetime(t, td)
	requireHandoffSuccessorRefused(t, s)
}

func corruptHandoffLifetime(t *testing.T, td *TaskDir) {
	t.Helper()
	path := filepath.Join(td.Dir, lifetimeDirectory, lifetimeTerminationName)
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte("{"), 0o600))
}

func recordHandoffBudgetStop(t *testing.T, td *TaskDir) {
	t.Helper()
	permit, err := td.PrepareStop("budget", "budget", time.Now().Add(time.Minute))
	must(t, err)
	must(t, permit.Consume())
	must(t, permit.Release())
	must(t, td.RecordStopReply("budget", task.StopReplyFacts{NumericTaskID: 7, Action: "kill", Acknowledged: true}))
	must(t, td.RecordStopObservation("budget", task.StopObservationFacts{NumericTaskID: 7, State: "ended", Terminated: true}))
}

func sealHandoffTask(t *testing.T, td *TaskDir) {
	t.Helper()
	start := consumeStart(t, td)
	must(t, td.RecordStarted(0))
	must(t, td.WriteRawFiles("answer\n", ""))
	_, err := td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
	must(t, err)
	must(t, start.Release())
}

func publishHandoffOutcome(t *testing.T, td *TaskDir) *task.OutcomeRecord {
	t.Helper()
	outcome, cleanupErr, collectErr := td.Collect(task.FixturePredicateRef())
	if cleanupErr != nil || collectErr != nil || outcome == nil {
		t.Fatalf("publish outcome = %+v cleanup=%v collect=%v", outcome, cleanupErr, collectErr)
	}
	return outcome
}

func requireHandoffReleaseBytes(t *testing.T, td *TaskDir, want []byte) {
	t.Helper()
	if got := readTestFile(t, handoffReleasePath(td)); !bytes.Equal(got, want) {
		t.Fatalf("release bytes changed:\n got %s\nwant %s", got, want)
	}
}
