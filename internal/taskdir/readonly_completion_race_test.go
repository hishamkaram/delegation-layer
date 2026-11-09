package taskdir

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestReadOnlyEvidenceCannotPairNewClosureWithOldPublication(t *testing.T) {
	for _, publish := range []bool{false, true} {
		t.Run(map[bool]string{false: "sealed", true: "committed"}[publish], func(t *testing.T) { checkReadOnlyCompletionRace(t, publish) })
	}
}

func checkReadOnlyCompletionRace(t *testing.T, publish bool) {
	t.Helper()
	store := testStore(t)
	td := preparedTask(t, store)
	owner, err := td.AcquireLifetimeOwner(t.Context())
	must(t, err)
	t.Cleanup(func() { must(t, owner.Release()) })
	registration := prepareCompletionTestIntent(t, td)
	_, err = td.RecordLifetimeRegistrationContext(t.Context(), registration)
	must(t, err)
	worker, err := td.RecordLifetimeWorkerContext(t.Context(), LifetimeWorkerFacts{PID: 4242, ProcessStart: "readonly-completion-worker", BootID: registration.BootID})
	must(t, err)
	workerDigest, err := td.LifetimeWorkerDigestContext(t.Context())
	must(t, err)
	permit := consumeStart(t, td)
	t.Cleanup(func() { must(t, permit.Release()) })
	must(t, td.RecordStarted(0))
	must(t, td.WriteRawFiles("answer", ""))
	reader, err := OpenReadOnlyStoreContext(t.Context(), store.Root, predicate.Default())
	must(t, err)
	t.Cleanup(func() { must(t, reader.Close()) })
	transitioned := false
	// Existing context checkpoints gate one normal writer transition after the
	// reader's entry check. This uses real durable writers, not timing or a fake
	// closure result: sealing/publication precedes the owner's native worker Wait.
	ctx := &lifetimeErrHookContext{Context: t.Context(), onErr: func(call int) {
		if call != 2 {
			return
		}
		transitioned = true
		_, writeErr := td.RecordLifetimeTerminationContext(t.Context(), task.LifetimeTerminationCompleted)
		must(t, writeErr)
		_, writeErr = td.Seal(task.InvocationStarted, 0, "", task.FixturePredicateRef())
		must(t, writeErr)
		if publish {
			_, cleanupErr, finalizeErr := td.Finalize(task.FixturePredicateRef())
			must(t, cleanupErr)
			must(t, finalizeErr)
		}
		must(t, permit.Release())
		exit := 0
		_, writeErr = td.RecordLifetimeWorkerWaitContext(t.Context(), LifetimeWorkerWaitFacts{WorkerSHA256: workerDigest, ChildPID: worker.PID, Result: task.LifetimeWorkerWaitSuccess, ExitCode: &exit})
		must(t, writeErr)
	}}
	evidence, err := reader.ReadTaskEvidence(ctx, td.TaskID)
	must(t, err)
	if !transitioned {
		t.Fatal("completion transition was not exercised")
	}
	closure, err := task.ResolveLifetimeClosure(evidence.Lifetime)
	must(t, err)
	if closure.Closed() && (evidence.Inspection == nil || !evidence.Inspection.SealExists) {
		t.Fatal("new closed lifetime was paired with an older no-seal publication view")
	}
	if publish && (evidence.Inspection.Outcome == nil || evidence.Inspection.Publication != task.PublicationCommitted) {
		t.Fatal("publication committed before worker exit was lost")
	}
}
