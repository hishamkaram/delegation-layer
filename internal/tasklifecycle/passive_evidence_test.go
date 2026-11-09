package tasklifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestPassiveReadinessDoesNotTreatStopIntentOrSealAsClosure(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*taskdir.TaskEvidence)
	}{
		{"undetermined", func(*taskdir.TaskEvidence) {}},
		{"sealed", func(f *taskdir.TaskEvidence) {
			f.Seal = &task.ProviderExitRecord{}
			f.Inspection.Publication = task.PublicationPending
		}},
		{"cancel-intent", func(f *taskdir.TaskEvidence) {
			f.Stops = []taskdir.StopRecord{{Request: &task.StopRequestRecord{Cause: "user"}}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			facts := taskdir.TaskEvidence{Exists: true, Inspection: &taskdir.TaskInspection{}}
			test.edit(&facts)
			observed, err := ClassifyPassiveTaskEvidence(facts, "")
			if err != nil || observed.Ready || observed.Liveness != task.LivenessUndetermined || observed.Readiness != PassivePending {
				t.Fatalf("unproven task became ready: %+v, %v", observed, err)
			}
		})
	}
}

func TestPassivePublicationReadinessNeedsOutcome(t *testing.T) {
	facts := taskdir.TaskEvidence{Exists: true, Inspection: &taskdir.TaskInspection{Publication: task.PublicationCommitted}}
	if _, err := ClassifyPassiveTaskEvidence(facts, ""); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("unbacked publication accepted: %v", err)
	}
	facts.Inspection.Outcome = &task.OutcomeRecord{}
	observed, err := ClassifyPassiveTaskEvidence(facts, "")
	if err != nil || !observed.Ready || observed.Readiness != PassivePublished || observed.Liveness != task.LivenessUndetermined {
		t.Fatalf("committed outcome not ready with unknown liveness: %+v, %v", observed, err)
	}
}

func TestPassiveNeverStartedProofCannotOverrideExecution(t *testing.T) {
	facts := taskdir.TaskEvidence{Exists: true, Started: &task.ProviderStartedRecord{}}
	if _, err := ClassifyPassiveTaskEvidence(facts, strings.Repeat("a", 64)); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatalf("never-started reference overrode provider start: %v", err)
	}
}

type passiveReaderFake struct {
	facts taskdir.TaskEvidence
	proof bool
	err   error
	calls int
}

func (r *passiveReaderFake) ReadTaskEvidence(context.Context, string) (taskdir.TaskEvidence, error) {
	return r.facts, r.err
}

func (r *passiveReaderFake) ReadNeverStartedTaskContext(context.Context, string) (bool, string, error) {
	r.calls++
	if r.proof {
		return true, strings.Repeat("a", 64), nil
	}
	return false, "", nil
}

func TestPassiveLoaderUsesCompleteProofAndStopsAtReadFault(t *testing.T) {
	reader := &passiveReaderFake{facts: taskdir.TaskEvidence{Exists: true}, proof: true}
	_, observed, err := ReadPassiveTaskObservation(t.Context(), reader, "11111111111111111111111111111111")
	if err != nil || !observed.Ready || observed.Readiness != PassiveNeverStarted || reader.calls != 1 {
		t.Fatalf("complete proof not used: %+v, %v calls=%d", observed, err, reader.calls)
	}
	reader.err, reader.calls = task.ErrIdentityMismatch, 0
	_, _, err = ReadPassiveTaskObservation(t.Context(), reader, "11111111111111111111111111111111")
	if !errors.Is(err, task.ErrIdentityMismatch) || reader.calls != 0 {
		t.Fatalf("read fault was retried or hidden: %v calls=%d", err, reader.calls)
	}
}
