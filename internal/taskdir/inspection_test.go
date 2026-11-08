package taskdir

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestTaskInspectionTerminal(t *testing.T) {
	var missing *TaskInspection
	if missing.Terminal() {
		t.Fatal("missing inspection was terminal")
	}
	if (&TaskInspection{Publication: task.PublicationPending}).Terminal() {
		t.Fatal("pending inspection was terminal")
	}
	if !(&TaskInspection{Publication: task.PublicationRejected}).Terminal() {
		t.Fatal("rejected inspection lost terminal publication")
	}
}
