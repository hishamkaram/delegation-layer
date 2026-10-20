package control

import (
	"context"
	"slices"
	"testing"
)

// privateMaintenancePassRun is one opportunistic maintenance attempt observed
// by a test. MaintainPrivateQueue reports only availability or coalescing, so
// a removal assertion requires this attempt's own skipped and error result.
type privateMaintenancePassRun struct {
	// ctx is the derived finite pass budget. It bounds every wait on the pass.
	ctx     context.Context
	done    chan struct{}
	skipped bool
	err     error
}

// startPrivateMaintenancePass admits the attempt under the client's private
// maintenance work token, as MaintainPrivateQueue does, because the attempt's
// worker client carries no work owner. Cleanup releases the caller's gates,
// cancels and joins the attempt, and keeps the token until every pending
// command returned by the attempt exits naturally, so the fixture client
// closes only after the pass releases its resources.
func startPrivateMaintenancePass(t *testing.T, client *Client, release func()) *privateMaintenancePassRun {
	t.Helper()
	finish, _, available := client.beginPrivateMaintenance()
	if finish == nil {
		t.Fatalf("private maintenance work was not admitted: available=%t", available)
	}
	ctx, cancel := context.WithTimeout(t.Context(), privateQueueMaintenanceTimeout(client.options.ObservationTimeout))
	pass := &privateMaintenancePassRun{ctx: ctx, done: make(chan struct{})}
	t.Cleanup(func() {
		release()
		cancel()
		<-pass.done
		if !retainPrivateQueueWorkUntilPending(pass.err, finish) {
			finish()
		}
	})
	go func() {
		defer close(pass.done)
		pass.skipped, pass.err = newPrivateQueueMaintenanceRun(client, false).attempt(ctx)
	}()
	return pass
}

// awaitBeforeCompletion waits within the pass budget for an event that must
// happen while the pass is still running.
func (pass *privateMaintenancePassRun) awaitBeforeCompletion(t *testing.T, event <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-event:
	case <-pass.done:
		t.Fatalf("maintenance finished before %s: skipped=%t err=%v", name, pass.skipped, pass.err)
	case <-pass.ctx.Done():
		t.Fatalf("maintenance did not reach %s within its derived budget: %v", name, pass.ctx.Err())
	}
}

// requireCompleted fails unless the attempt won the maintenance election and
// finished its pass without error within the derived budget.
func (pass *privateMaintenancePassRun) requireCompleted(t *testing.T) {
	t.Helper()
	select {
	case <-pass.done:
	case <-pass.ctx.Done():
		select {
		case <-pass.done:
		default:
			t.Fatalf("private maintenance pass exceeded its derived budget: %v", pass.ctx.Err())
		}
	}
	if pass.skipped || pass.err != nil {
		t.Fatalf("private maintenance pass did not complete: skipped=%t err=%v", pass.skipped, pass.err)
	}
}

// assertDeferredMaintenanceStatusOnly proves that a deferred opportunistic
// pass issued only its held status command, which then exited naturally.
func assertDeferredMaintenanceStatusOnly(t *testing.T, events []CommandEvent) {
	t.Helper()
	stages := make([]string, 0, len(events))
	for _, event := range events {
		if event.CommandID != events[0].CommandID || !slices.Contains(event.Argv, "status") {
			t.Fatalf("deferred maintenance issued another command: %+v", events)
		}
		stages = append(stages, event.Stage)
	}
	if !slices.Equal(stages, []string{"entry", "started", "completed"}) || events[len(events)-1].ExitCode != 0 {
		t.Fatalf("deferred maintenance status events=%+v, want one natural successful completion", events)
	}
}
