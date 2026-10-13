package lifetime

import "testing"

func TestProcessGroupPlanIsDeterministicAndOwnerScoped(t *testing.T) {
	first := processGroupPlan("root", "task", "task-id")
	second := processGroupPlan("root", "task", "task-id")
	inspection := processGroupPlan("root", "inspection", "task-id")
	if first != second {
		t.Fatal("same durable identity produced different plans")
	}
	if first == inspection {
		t.Fatal("task and inspection plans share a namespace")
	}
	if err := validatePlanShape(first); err != nil {
		t.Fatalf("validate process-group plan: %v", err)
	}
}

func TestNewPlanRejectsInvalidShape(t *testing.T) {
	tests := []Plan{
		{},
		{kind: KindCgroupV2, assurance: AssuranceCooperative, identifier: "bad"},
		{kind: KindProcessGroup, assurance: AssuranceExact, identifier: "pg:00000000000000000000000000000000"},
		{kind: KindProcessGroup, assurance: AssuranceCooperative, identifier: "bad"},
	}
	for _, test := range tests {
		if _, err := NewPlan(test.kind, test.assurance, test.identifier); err == nil {
			t.Fatalf("NewPlan accepted %#v", test)
		}
	}
}

func TestOpenRegisteredProcessGroupRequiresExactPlan(t *testing.T) {
	plan := processGroupPlan("root", "task", "task-id")
	ref, err := NewRef(plan.Kind(), plan.Assurance(), plan.Identifier())
	if err != nil {
		t.Fatal(err)
	}
	scope, err := OpenRegistered(plan, ref)
	if err != nil {
		t.Fatalf("OpenRegistered: %v", err)
	}
	if err = scope.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	other := processGroupPlan("root", "inspection", "task-id")
	if _, err = OpenRegistered(other, ref); err == nil {
		t.Fatal("OpenRegistered accepted a reference from another plan")
	}
}
