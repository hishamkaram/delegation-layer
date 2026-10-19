package workspaces

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func TestWorkspaceControlTransactionNestedCallsRetainOneCutoff(t *testing.T) {
	observer, cancelObserver := context.WithCancel(t.Context())
	cancelObserver()
	control, finish := workspaceSchedulingControl(observer, OperationOptions{})
	defer finish()
	cutoff, finite := control.Deadline()
	if !finite || control.Err() != nil {
		t.Fatal("coordinator inherited an expired observer instead of its finite control cutoff")
	}
	nested, closeNested := workspaceSchedulingControl(control, OperationOptions{ObservationTimeout: time.Hour})
	defer closeNested()
	nestedCutoff, finite := nested.Deadline()
	if !finite || !nestedCutoff.Equal(cutoff) {
		t.Fatal("nested scheduling renewed the control transaction")
	}
	finish()
	if nested.Err() != context.Canceled {
		t.Fatal("nested scheduling detached from its control owner")
	}
	replayed, closeReplay := workspaceSchedulingControl(control, OperationOptions{})
	defer closeReplay()
	replayedCutoff, finite := replayed.Deadline()
	if !finite || !replayedCutoff.Equal(cutoff) || replayed.Err() != context.Canceled {
		t.Fatal("nested replay revived an already-canceled control transaction")
	}
}

func TestWorkspaceControlTransactionNestedCallsKeepEarlierHelperDeadline(t *testing.T) {
	control, finish := workspaceSchedulingControl(t.Context(), OperationOptions{})
	defer finish()
	cutoff, _ := control.Deadline()
	helperDeadline := cutoff.Add(-time.Second)
	helper, closeHelper := context.WithDeadline(control, helperDeadline)
	defer closeHelper()
	nested, closeNested := workspaceSchedulingControl(helper, OperationOptions{})
	defer closeNested()
	deadline, finite := nested.Deadline()
	if !finite || !deadline.Equal(helperDeadline) {
		t.Fatal("nested scheduling escaped the original helper deadline")
	}
}

// The short observation budget bounds the whole control transaction, not one
// call: the cutoff is fixed once and neither elapsed time nor a larger nested
// budget may move it.
func TestWorkspaceControlTransactionCutoffIsFixedAcrossElapsedTime(t *testing.T) {
	const observation = 10 * time.Millisecond
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		wantCutoff := started.Add(pueue.PrivateControlTimeout(pueue.Options{ObservationTimeout: observation}))
		observer, cancelObserver := context.WithCancel(t.Context())
		cancelObserver()
		control, finish := workspaceSchedulingControl(observer, OperationOptions{ObservationTimeout: observation})
		defer finish()
		cutoff, finite := control.Deadline()
		if !finite || !cutoff.Equal(wantCutoff) || control.Err() != nil {
			t.Fatalf("initial control cutoff = %v (finite=%t, err=%v), want %v", cutoff, finite, control.Err(), wantCutoff)
		}

		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		if !wantCutoff.After(time.Now()) || control.Err() != nil {
			t.Fatalf("test clock overshot the control cutoff: now=%v cutoff=%v err=%v", time.Now(), wantCutoff, control.Err())
		}
		nested, closeNested := workspaceSchedulingControl(control, OperationOptions{ObservationTimeout: time.Hour})
		defer closeNested()
		if nestedCutoff, finite := nested.Deadline(); !finite || !nestedCutoff.Equal(wantCutoff) {
			t.Fatalf("nested scheduling renewed the cutoff to %v, want %v", nestedCutoff, wantCutoff)
		}
		helperDeadline := time.Now().Add(10 * time.Millisecond)
		helper, closeHelper := context.WithDeadline(control, helperDeadline)
		defer closeHelper()
		earlier, closeEarlier := workspaceSchedulingControl(helper, OperationOptions{ObservationTimeout: time.Hour})
		defer closeEarlier()
		if earlierDeadline, finite := earlier.Deadline(); !finite || !earlierDeadline.Equal(helperDeadline) {
			t.Fatalf("nested scheduling escaped the earlier helper deadline: %v, want %v", earlierDeadline, helperDeadline)
		}

		time.Sleep(time.Until(wantCutoff) + time.Millisecond)
		synctest.Wait()
		if !errors.Is(control.Err(), context.DeadlineExceeded) || !errors.Is(nested.Err(), context.DeadlineExceeded) {
			t.Fatalf("control transaction outlived its cutoff: control=%v nested=%v", control.Err(), nested.Err())
		}
		revived, closeRevived := workspaceSchedulingControl(control, OperationOptions{ObservationTimeout: 2 * time.Hour})
		defer closeRevived()
		if reviveCutoff, finite := revived.Deadline(); !finite || !reviveCutoff.Equal(wantCutoff) || !errors.Is(revived.Err(), context.DeadlineExceeded) {
			t.Fatalf("larger nested budget revived an expired control transaction: cutoff=%v err=%v", reviveCutoff, revived.Err())
		}
	})
}
