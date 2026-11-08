package app

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/resourceadmission"
)

func TestParseArgumentsResourceGuards(t *testing.T) {
	commands := [][]string{
		{"dispatch", "--provider", "claude:print", "--brief", "/brief", "--cwd", "/work", "--budget", "1m"},
		{"continue", "--task", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--budget", "1m"},
	}
	cases := []struct {
		flags []string
		want  resourceadmission.GuardPolicy
	}{
		{},
		{[]string{"--cpu-guard=false"}, resourceadmission.GuardPolicy{DisableCPU: true}},
		{[]string{"--memory-guard=false"}, resourceadmission.GuardPolicy{DisableMemory: true}},
		{[]string{"--cpu-guard=false", "--memory-guard=false"}, resourceadmission.GuardPolicy{DisableCPU: true, DisableMemory: true}},
		{[]string{"--cpu-guard=true", "--memory-guard=true"}, resourceadmission.GuardPolicy{}},
	}
	for _, command := range commands {
		for _, tc := range cases {
			args := append(append([]string(nil), command...), tc.flags...)
			got, err := ParseArguments(args)
			if err != nil || got.ResourceGuards != tc.want {
				t.Errorf("%v: policy=%+v err=%v want%+v", args, got.ResourceGuards, err, tc.want)
			}
		}
	}
	if (Arguments{}).ResourceGuards != (resourceadmission.GuardPolicy{}) {
		t.Fatal("zero arguments disabled guards")
	}
}

func TestParseArgumentsRejectResourceGuardsOutsideAdmission(t *testing.T) {
	for _, command := range [][]string{{"preflight"}, {"models"}, {"providers"}, {"status", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, {"supervisor", "status"}, {"scheduler", "serve"}, {"schedule", "list"}, {"workspace", "list"}, {"remote", "list"}, {"devices", "list"}} {
		for _, flag := range []string{"--cpu-guard=false", "--memory-guard=true"} {
			args := append(append([]string(nil), command...), flag)
			if _, err := ParseArguments(args); err == nil {
				t.Errorf("unsupported command accepted %v", args)
			}
		}
	}
	for _, flag := range []string{"--cpu-guard=invalid", "--memory-guard=invalid"} {
		if _, err := ParseArguments([]string{"continue", "--task", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", flag}); err == nil {
			t.Errorf("invalid flag accepted %s", flag)
		}
	}
}

func TestResourceGuardDisabledEvidenceIsExplicitAndDerived(t *testing.T) {
	policy := resourceadmission.GuardPolicy{DisableCPU: true, DisableMemory: true}
	assessment, err := resourceadmission.NewChecker(policy).Check(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	response := resourceAdmissionResponse(&assessment, nil)
	if response.Status != ResourceAdmissionClear || !reflect.DeepEqual(response.DisabledGuards, []string{"cpu", "memory"}) || len(response.Warnings) != 0 {
		t.Fatalf("projection=%+v", response)
	}
}

func TestResourceGuardEvidenceKeepsDefaultWireAndHumanOptOut(t *testing.T) {
	assessment := resourceadmission.NewAssessment(time.Now(), resourceadmission.Scope{}, resourceadmission.Findings{})
	encoded, err := json.Marshal(resourceAdmissionResponse(&assessment, nil))
	if err != nil || string(encoded) != `{"status":"clear"}` {
		t.Fatalf("default wire changed: %s %v", encoded, err)
	}
	assessment, err = resourceadmission.NewChecker(resourceadmission.GuardPolicy{DisableCPU: true, DisableMemory: true}).Check(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	writeHumanResourceAdmission(&output, resourceAdmissionResponse(&assessment, nil))
	if output.String() != "resource_admission=clear\nresource_disabled_guards=cpu,memory\n" {
		t.Fatalf("human opt-out evidence=%q", output.String())
	}
}
