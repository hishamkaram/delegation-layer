package inspection

import (
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInspectionRequestWorkspaceBindingIsAnIndependentSnapshot(t *testing.T) {
	original := task.TaskRecord{Workspace: &task.WorkspaceBinding{ID: strings.Repeat("a", 32), Reservation: 1, RequestSHA256: task.ComputeSHA256(nil), BudgetNanos: 1}}
	copy := cloneTaskRecord(original)
	copy.Workspace.Reservation++
	if original.Workspace.Reservation != 1 {
		t.Fatal("mutating a request snapshot changed its caller's workspace use")
	}
}
