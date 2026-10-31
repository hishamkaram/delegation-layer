package taskdir

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

type automaticRefusalFault struct {
	BaseFaultInjector
	target  string
	phase   string
	fired   bool
	failure error
}

func (f *automaticRefusalFault) OnLink(_, destination string) error {
	if destination == f.target && f.phase == "absent" && !f.fired {
		f.fired = true
		return f.failure
	}
	return nil
}

func (f *automaticRefusalFault) OnAfterLinkDirBarrier(destination string) error {
	if destination != f.target || f.phase == "absent" || f.fired {
		return nil
	}
	f.fired = true
	switch f.phase {
	case "unreadable":
		return errors.Join(f.failure, os.WriteFile(destination, []byte("unreadable refusal"), 0o600))
	case "conflicting":
		data, err := os.ReadFile(destination)
		if err != nil {
			return err
		}
		var rival task.StopRequestRecord
		if err = task.DecodeStrict(data, &rival); err != nil {
			return err
		}
		rival.RequestedAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
		data, err = task.MarshalCanonical(rival)
		if err != nil {
			return err
		}
		return errors.Join(f.failure, os.WriteFile(destination, data, 0o600))
	default:
		return f.failure
	}
}

func TestAutomaticFenceAmbiguousWritesReadBackThroughProductionStore(t *testing.T) {
	for _, phase := range []string{"absent", "matching", "conflicting", "unreadable"} {
		t.Run(phase, func(t *testing.T) {
			store := testStore(t)
			td := supervisorTask(t, store)
			id := task.ComputeSHA256([]byte(task.StopCauseAutomaticAbandonment + ":" + td.TaskID))[:32]
			injected := errors.New("injected refusal persistence failure")
			hook := &automaticRefusalFault{target: filepath.Join(td.Dir, "stop", id+stopRequestSuffix), phase: phase, failure: injected}
			store.SetFaultInjector(hook)
			proven, digest, err := td.FenceUnlaunchedTaskContext(t.Context())
			if proven || digest != "" || !errors.Is(err, injected) {
				t.Fatalf("ambiguous %s granted proof: %v %q %v", phase, proven, digest, err)
			}
			if !hook.fired {
				t.Fatal("persistence injection did not run")
			}
			if phase == "conflicting" && !errors.Is(err, task.ErrIdentityMismatch) {
				t.Fatalf("conflict not classified: %v", err)
			}
			if phase == "unreadable" {
				proven, digest, err = td.FenceUnlaunchedTaskContext(t.Context())
				if proven || digest != "" || err == nil {
					t.Fatalf("unreadable winner granted proof: %v %q %v", proven, digest, err)
				}
				return
			}
			assertAutomaticFenceRecoversWinner(t, td, id)
		})
	}
}

func assertAutomaticFenceRecoversWinner(t *testing.T, td *TaskDir, id string) {
	t.Helper()
	before, err := td.ReadStopRequest(id)
	must(t, err)
	proven, digest, err := td.FenceUnlaunchedTaskContext(t.Context())
	must(t, err)
	if !proven || digest == "" {
		t.Fatalf("durable winner not recovered: %v %q", proven, digest)
	}
	after, err := td.ReadStopRequest(id)
	must(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("recovery replaced immutable winner")
	}
	assertNoFabricatedExecution(t, td)
}

func TestAutomaticAbandonmentRequiresConditionalWriterAndTruthfulFields(t *testing.T) {
	td := supervisorTask(t, testStore(t))
	if permit, err := td.PrepareStop(td.TaskID, task.StopCauseAutomaticAbandonment, time.Time{}); permit != nil || !errors.Is(err, task.ErrInvalidPermit) {
		t.Fatalf("general stop path asserted abandonment: %v %v", permit, err)
	}
	proven, _, err := td.FenceUnlaunchedTaskContext(t.Context())
	must(t, err)
	if !proven {
		t.Fatal("conditional writer did not prove refusal")
	}
	rows, err := td.ReadStopRecords()
	must(t, err)
	original := *rows[0].Request
	for _, field := range []string{"deadline", "target", "budget-id", "unknown-cause"} {
		candidate := original
		switch field {
		case "deadline":
			candidate.Deadline = time.Now().UTC().Format(time.RFC3339Nano)
		case "target":
			id := int64(7)
			candidate.NumericTaskID = &id
		case "budget-id":
			candidate.RequestID = "budget"
		case "unknown-cause":
			candidate.Cause = "unknown"
		}
		if err = task.ValidateStopRequestRecord(&candidate); err == nil {
			t.Fatalf("invalid automatic refusal accepted: %s", field)
		}
	}
}
