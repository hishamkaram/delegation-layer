package task

import (
	"bytes"
	"strings"
	"testing"
)

func TestWorkspaceBindingIsOptionalAndPartOfRequestIdentity(t *testing.T) {
	ordinary := fixtureTask(t)
	data, err := MarshalCanonical(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"workspace"`)) {
		t.Fatal("ordinary request encoding changed")
	}
	managed := ordinary
	managed.Workspace = &WorkspaceBinding{ID: strings.Repeat("3", 32), Reservation: 1, RequestSHA256: ComputeSHA256([]byte("admission")), BudgetNanos: 1}
	if err = ValidateTaskRecord(&managed); err != nil {
		t.Fatal(err)
	}
	if CompareRequests(&ordinary, &managed) {
		t.Fatal("workspace use was ignored in request equality")
	}
	other := managed
	changed := *managed.Workspace
	changed.Reservation++
	other.Workspace = &changed
	if CompareRequests(&managed, &other) {
		t.Fatal("different workspace uses compare equal")
	}
}

func TestWorkspaceBindingRejectsInvalidUse(t *testing.T) {
	for _, binding := range []WorkspaceBinding{
		{ID: "invalid", Reservation: 1, RequestSHA256: ComputeSHA256(nil)},
		{ID: strings.Repeat("3", 32), RequestSHA256: ComputeSHA256(nil)},
		{ID: strings.Repeat("3", 32), Reservation: 1, RequestSHA256: "invalid"},
	} {
		request := fixtureTask(t)
		request.Workspace = &binding
		if err := ValidateTaskRecord(&request); err == nil {
			t.Fatalf("accepted invalid workspace binding: %+v", binding)
		}
	}
}
