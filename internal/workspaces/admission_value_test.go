package workspaces

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestAdmissionBindingRetainsOwnedIdentity(t *testing.T) {
	want := task.WorkspaceBinding{ID: "workspace", Reservation: 7, RequestSHA256: "request", BudgetNanos: 123}
	owned := want
	owner := &Admission{binding: &owned}
	observed := owner.Binding()
	if observed == nil || *observed != want {
		t.Fatalf("binding observation lost owned identity: %v", observed)
	}
	observed.Reservation = 99
	observed.RequestSHA256 = "changed"
	if got := owner.Binding(); got == nil || *got != want {
		t.Fatalf("caller mutation changed owned admission: %v", got)
	}
	if (*Admission)(nil).Binding() != nil || (&Admission{}).Binding() != nil {
		t.Fatal("absent admission fabricated a binding")
	}
}
