package workspaces

import (
	"context"
	"testing"
	"time"
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
