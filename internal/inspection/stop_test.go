package inspection

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

func TestInspectionStopRequiresSavedTarget(t *testing.T) {
	op := evidenceOperation(t)
	if permit, _, err := op.ClaimStop(op.Deadline()); permit != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stop without saved target authorized")
	}
	mustInspection(t, op.RecordReceipt(42))
	if permit, _, err := op.ClaimStop(op.Deadline()); err != nil || permit == nil {
		t.Fatalf("saved target did not authorize stop: permit=%v err=%v", permit, err)
	}
}

func TestInspectionStopRequiresExactDeadline(t *testing.T) {
	op := evidenceOperation(t)
	mustInspection(t, op.RecordReceipt(42))
	if permit, _, err := op.ClaimStop(op.Deadline().Add(time.Second)); permit != nil || !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatal("changed deadline authorized")
	}
}

func TestInspectionStopPermitBindsTargetAndIsOneShot(t *testing.T) {
	op := evidenceOperation(t)
	mustInspection(t, op.RecordReceipt(42))
	permit, request, err := op.ClaimStop(op.Deadline())
	mustInspection(t, err)
	if request.NumericTaskID != 42 || request.Supervisor != op.Request().Binding.Supervisor {
		t.Fatal("stop target binding lost")
	}
	if permit.InspectionStopRootID() != op.Request().RootID || permit.InspectionStopTaskID() != op.Request().TaskID || permit.InspectionStopSupervisor() != op.Request().Binding.Supervisor {
		t.Fatalf("stop permit lost immutable request binding: root=%q task=%q supervisor=%+v", permit.InspectionStopRootID(), permit.InspectionStopTaskID(), permit.InspectionStopSupervisor())
	}
	target := permit.InspectionStopNumericTaskID()
	if target == nil || *target != 42 {
		t.Fatalf("stop permit target missing: %v", target)
	}
	*target = 43
	if fresh := permit.InspectionStopNumericTaskID(); fresh == nil || *fresh != 42 {
		t.Fatalf("stop permit target was not defensively copied: %v", fresh)
	}
	mustInspection(t, permit.Consume())
	if !errors.Is(permit.Consume(), task.ErrPermitAlreadyUsed) {
		t.Fatal("stop permit reusable")
	}
	if next, _, claimErr := op.ClaimStop(op.Deadline()); next != nil || !errors.Is(claimErr, ErrStopAlreadyClaimed) {
		t.Fatal("replayed stop granted authority")
	}
}

func TestInspectionStopAcknowledgmentNeverImpliesTermination(t *testing.T) {
	op := evidenceOperation(t)
	mustInspection(t, op.RecordReceipt(42))
	_, _, err := op.ClaimStop(op.Deadline())
	mustInspection(t, err)
	if err = op.RecordStopReply(43, "kill", true); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatal("wrong acknowledgment target accepted")
	}
	if err = op.RecordStopReply(42, "retry", true); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatal("unknown stop action accepted")
	}
	mustInspection(t, op.RecordStopReply(42, "kill", true))
	var observation StopObservationRecord
	if err = op.control.Read(stopObservationRecordName, &observation); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("acknowledgment invented termination")
	}
	if err = op.RecordStopObservation(43); !errors.Is(err, task.ErrEvidenceFault) {
		t.Fatal("wrong termination target accepted")
	}
	mustInspection(t, op.RecordStopObservation(42))
	mustInspection(t, op.control.Read(stopObservationRecordName, &observation))
	if observation.State != "ended" || observation.NumericTaskID != 42 {
		t.Fatal("independent observation lost")
	}
}

func TestInspectionStopBlocksEligibilityEvenAfterClockMovesBackward(t *testing.T) {
	op := evidenceOperation(t)
	_, err := op.ClaimStart(time.Unix(101, 0))
	mustInspection(t, err)
	mustInspection(t, op.RecordReceipt(42))
	_, _, err = op.ClaimStop(op.Deadline())
	mustInspection(t, err)
	if err = op.Complete(ResultEligible, []byte(`{"eligible":true}`), time.Unix(102, 0)); !errors.Is(err, ErrAdmissionExpired) {
		t.Fatal("stop intent lost to earlier wall-clock timestamp")
	}
}

func TestInspectionUncertainStopNeverGrantsAnotherPermit(t *testing.T) {
	store, request, binding := inspectionFixture(t)
	op, err := OpenOperation(store, request, binding, time.Unix(100, 0))
	mustInspection(t, err)
	t.Cleanup(func() { mustInspection(t, op.Close()) })
	mustInspection(t, op.RecordReceipt(42))
	store.SetFaultInjector(&stopPostLinkFault{})
	permit, _, err := op.ClaimStop(op.Deadline())
	if permit != nil || !errors.Is(err, task.ErrUncertainDurability) {
		t.Fatal("uncertain stop granted authority")
	}
	store.SetFaultInjector(nil)
	permit, _, err = op.ClaimStop(op.Deadline())
	if permit != nil || !errors.Is(err, ErrStopAlreadyClaimed) {
		t.Fatal("uncertain stop was retried")
	}
}

type stopPostLinkFault struct{ taskdir.BaseFaultInjector }

func (*stopPostLinkFault) OnPostLinkDirBarrier(path string) error {
	if filepath.Base(path) == stopRequestRecordName {
		return errors.New("injected stop journal barrier failure")
	}
	return nil
}
