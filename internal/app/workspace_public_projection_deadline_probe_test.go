package app

import (
	"reflect"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Public status and collect of a committed managed task keep their task
// envelope when their own bounded workspace view expires behind another holder
// of the workspace control lock; the unobserved workspace stays undetermined.
func TestManagedPublicTaskWorkspaceViewDeadlineRemainsUndetermined(t *testing.T) {
	for _, observer := range []struct {
		name    string
		observe func(Arguments) commandResult
	}{
		{name: "status", observe: func(a Arguments) commandResult { return status(a, Dependencies{}) }},
		{name: "collect", observe: func(a Arguments) commandResult { return collect(a, storeDependencies{}) }},
	} {
		t.Run(observer.name, func(t *testing.T) {
			requirePublicWorkspaceViewDeadline(t, observer.observe)
		})
	}
}

func requirePublicWorkspaceViewDeadline(t *testing.T, observe func(Arguments) commandResult) {
	store, use := newCommittedManagedTask(t)
	arguments := Arguments{Root: store.Root, TaskID: use.td.TaskID}

	baseline := observe(arguments)
	requireCommittedTaskEnvelope(t, baseline)
	view := baseline.response.Workspace
	requireReadableNonretainedView(t, view)
	if continuation := baseline.response.Continuation; continuation != nil && continuation.Resumable {
		t.Fatalf("nonretained workspace offered continuation: %+v", continuation)
	}
	// Measure observational immutability after the first observation has
	// initialized the canonical use-owner lease it shares with later ones.
	records := storeRecordDigests(t, store.Root)
	envelope := taskProjectionWithoutWorkspace(t, baseline)

	releaseLock, lockReleased := holdWorkspaceControlLock(t, store, view.ID)
	contended := observe(arguments)
	requireLockStillHeld(t, lockReleased)
	requireUnobservedWorkspaceView(t, contended.response.Workspace, view)
	requireSameTaskEnvelope(t, "contended", baseline, contended, envelope)
	requireUnchangedRecords(t, "contended observation", store.Root, records)

	releaseLock()
	<-lockReleased
	recovered := observe(arguments)
	if !reflect.DeepEqual(recovered.response.Workspace, view) {
		t.Errorf("released view differs from the baseline: %+v", recovered.response.Workspace)
	}
	requireSameTaskEnvelope(t, "recovered", baseline, recovered, envelope)
	requireUnchangedRecords(t, "recovered observation", store.Root, records)
}

// newCommittedManagedTask publishes the task's committed outcome through the
// canonical fixture predicate and then seals its own workspace capture, so
// later public observations have no pending collection or capture work.
func newCommittedManagedTask(t *testing.T) (*taskdir.Store, appTaskUse) {
	t.Helper()
	store, resource, use := newManagedTaskUse(t)
	if _, err := sealFixtureEvidence(use.td, task.FixturePredicateRef(), "answer\n", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	sealAppTaskUse(t, resource, use, managedTaskCaptureID)
	return store, use
}

func requireCommittedTaskEnvelope(t *testing.T, result commandResult) {
	t.Helper()
	if result.code != 0 || result.err != nil || result.response.Publication != task.PublicationCommitted.String() || result.response.Outcome == nil || result.response.Payload == nil {
		t.Fatalf("committed baseline: code=%d err=%v publication=%q", result.code, result.err, result.response.Publication)
	}
}
