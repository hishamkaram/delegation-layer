package contract

import (
	"testing"
	"time"
)

func TestResultFactAcceptsExistingDiagnosticCodeContract(t *testing.T) {
	f := ResultFact{TaskID: testTaskID, ObservedAt: at(2000), Verdict: "rejected", Code: "adapter_v2_warning", OutcomeSHA256: digestOf('a')}
	if err := ValidateResultFact(f); err != nil {
		t.Fatalf("valid adapter code faulted schedule: %v", err)
	}
	f.Code = "provider\noutput"
	if err := ValidateResultFact(f); err == nil {
		t.Fatal("control bytes accepted")
	}
}

func TestDefinitionHookIsClonedAndBoundToDigest(t *testing.T) {
	d := testDefinition()
	d.Hook = &FailureHook{Executable: "/installed/hook", Threshold: 2, Timeout: time.Second}
	before, err := DefinitionDigests(d)
	if err != nil {
		t.Fatal(err)
	}
	copy := d.Clone()
	copy.Hook.Threshold = 3
	if d.Hook.Threshold != 2 {
		t.Fatal("hook clone aliases original")
	}
	after, err := DefinitionDigests(copy)
	if err != nil || before.DefinitionSHA256 == after.DefinitionSHA256 {
		t.Fatal("hook change not bound to definition digest")
	}
}
