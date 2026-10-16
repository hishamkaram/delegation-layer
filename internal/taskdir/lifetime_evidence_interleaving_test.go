package taskdir

import (
	"context"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// The hook runs after a validated leaf read, outside the store's transaction.
// It deterministically interleaves the real publisher with evidence assembly.
type interleavedLifetimeReader struct {
	LifetimeRecordReader
	boundary string
	publish  func()
}

func (r *interleavedLifetimeReader) after(boundary string) {
	if r.boundary == boundary && r.publish != nil {
		publish := r.publish
		r.publish = nil
		publish()
	}
}

func (r *interleavedLifetimeReader) ReadLifetimeRegistrationContext(ctx context.Context) (*task.LifetimeRegistrationRecord, error) {
	record, err := r.LifetimeRecordReader.ReadLifetimeRegistrationContext(ctx)
	r.after("registration")
	return record, err
}

func (r *interleavedLifetimeReader) ReadLifetimeWorkerContext(ctx context.Context) (*task.LifetimeWorkerRecord, error) {
	record, err := r.LifetimeRecordReader.ReadLifetimeWorkerContext(ctx)
	r.after("worker")
	return record, err
}

func (r *interleavedLifetimeReader) ReadLifetimeLaunchesContext(ctx context.Context) ([]task.LifetimeLaunchRecord, error) {
	records, err := r.LifetimeRecordReader.ReadLifetimeLaunchesContext(ctx)
	r.after("launches")
	return records, err
}

func (r *interleavedLifetimeReader) ReadLifetimeTerminationContext(ctx context.Context) (*task.LifetimeTerminationRecord, error) {
	record, err := r.LifetimeRecordReader.ReadLifetimeTerminationContext(ctx)
	r.after("termination")
	return record, err
}

type interleavingTestStore interface {
	completionTestStore
	RecordLifetimeLaunchContext(context.Context, LifetimeLaunchFacts) (*task.LifetimeLaunchRecord, error)
}

func publishInterleavingWorker(t *testing.T, records completionTestStore, bootID string) {
	t.Helper()
	_, err := records.RecordLifetimeWorkerContext(t.Context(), LifetimeWorkerFacts{PID: 1234, ProcessStart: "interleaved-worker", BootID: bootID})
	must(t, err)
}

func publishInterleavingWait(t *testing.T, records completionTestStore) {
	t.Helper()
	worker, err := records.ReadLifetimeWorkerContext(t.Context())
	must(t, err)
	data, err := task.MarshalCanonical(worker)
	must(t, err)
	exitCode := 0
	_, err = records.RecordLifetimeWorkerWaitContext(t.Context(), LifetimeWorkerWaitFacts{WorkerSHA256: task.ComputeSHA256(data), ChildPID: worker.PID, Result: task.LifetimeWorkerWaitSuccess, ExitCode: &exitCode})
	must(t, err)
}

func publishInterleavingLaunch(t *testing.T, records interleavingTestStore, sequence uint64) {
	t.Helper()
	_, err := records.RecordLifetimeLaunchContext(t.Context(), LifetimeLaunchFacts{Sequence: sequence, PID: 2345, ProcessStart: "interleaved-launch", PGID: 2345})
	must(t, err)
}

func prepareInterleaving(t *testing.T, records interleavingTestStore, boundary string) (func(), task.LifetimeClosureKind) {
	t.Helper()
	facts := prepareCompletionTestIntent(t, records)
	if boundary != "registration" {
		_, err := records.RecordLifetimeRegistrationContext(t.Context(), facts)
		must(t, err)
	}
	if boundary == "launches" || boundary == "termination" {
		publishInterleavingWorker(t, records, facts.BootID)
	}
	if boundary == "launches" {
		publishInterleavingLaunch(t, records, 1)
	}
	return func() {
		publishInterleavingCompletion(t, records, facts, boundary)
	}, interleavingClosure(boundary)
}

func interleavingClosure(boundary string) task.LifetimeClosureKind {
	if boundary == "termination" {
		return task.LifetimeClosureRegisteredRecovered
	}
	return task.LifetimeClosureRegisteredNative
}

func publishInterleavingCompletion(t *testing.T, records interleavingTestStore, facts LifetimeRegistrationFacts, boundary string) {
	t.Helper()
	if boundary == "registration" {
		_, err := records.RecordLifetimeRegistrationContext(t.Context(), facts)
		must(t, err)
	}
	if boundary == "registration" || boundary == "worker" {
		publishInterleavingWorker(t, records, facts.BootID)
	}
	if boundary == "launches" {
		publishInterleavingLaunch(t, records, 2)
	}
	_, err := records.RecordLifetimeTerminationContext(t.Context(), task.LifetimeTerminationCompleted)
	must(t, err)
	if boundary == "termination" {
		_, err = records.RecordLifetimeWorkerAbsenceContext(t.Context(), task.LifetimeAbsenceExactWorker, facts.BootID)
		must(t, err)
		return
	}
	publishInterleavingWait(t, records)
}

func TestLifetimeEvidenceConcurrentPublicationKeepsDependencies(t *testing.T) {
	for _, family := range []string{"task", "inspection", "workspace"} {
		for _, boundary := range []string{"registration", "worker", "launches", "termination"} {
			t.Run(family+"/"+boundary, func(t *testing.T) {
				records, _ := newCompletionTestStore(t, family)
				writer, ok := records.(interleavingTestStore)
				if !ok {
					t.Fatal("production fixture cannot publish launches")
				}
				owner, err := writer.AcquireLifetimeOwner(t.Context())
				must(t, err)
				t.Cleanup(func() { must(t, owner.Release()) })
				publish, wantClosure := prepareInterleaving(t, writer, boundary)
				reader := &interleavedLifetimeReader{LifetimeRecordReader: records, boundary: boundary, publish: publish}
				evidence, err := LoadLifetimeEvidenceContext(t.Context(), reader)
				must(t, err)
				closure, err := task.ResolveLifetimeClosure(evidence)
				must(t, err)
				if closure.Closed() || reader.publish != nil {
					t.Fatalf("interleaved read must be a conservative open prefix: %+v", closure)
				}
				evidence, err = LoadLifetimeEvidenceContext(t.Context(), records)
				must(t, err)
				closure, err = task.ResolveLifetimeClosure(evidence)
				must(t, err)
				if closure.Kind != wantClosure {
					t.Fatalf("quiescent closure = %q, want %q", closure.Kind, wantClosure)
				}
			})
		}
	}
}
