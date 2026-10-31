package taskdir

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestAutomaticAbandonmentRefusesFreshLaunchWithoutUserCancellation(t *testing.T) {
	td := supervisorTask(t, testStore(t))
	proven, _, err := td.FenceUnlaunchedTaskContext(t.Context())
	must(t, err)
	if !proven {
		t.Fatal("created task was not fenced")
	}
	refused, err := td.TaskLaunchRefusedContext(t.Context())
	must(t, err)
	if !refused {
		t.Fatal("automatic abandonment did not permanently refuse launch")
	}
	user, err := td.UserStopRequestedContext(t.Context())
	must(t, err)
	if user {
		t.Fatal("automatic abandonment fabricated a user cancellation")
	}
	_, meta, err := td.PreparedRecords()
	must(t, err)
	if _, err = td.PrepareSubmission(meta.SupervisorConfig); !errors.Is(err, task.ErrTerminalTask) {
		t.Fatalf("submission after abandonment: %v", err)
	}
	if _, err = td.PrepareStart(0); !errors.Is(err, task.ErrTerminalTask) {
		t.Fatalf("start after abandonment: %v", err)
	}
}
