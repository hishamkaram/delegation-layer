package maintainability

import (
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

func TestGoFunctionDiagnosticBindsExactMaintainedDeclaration(t *testing.T) {
	root := t.TempDir()
	issue := funlenIssue{FromLinter: "funlen", Text: "Function 'F' is too long (81 > 80)"}
	issue.Pos.Filename, issue.Pos.Line = "value.go", 3
	sources := []policy.Source{{Path: "value.go", Functions: []policy.Function{{Name: "F", Line: 3}}}}
	findings, err := funlenFindings(root, []funlenIssue{issue}, sources)
	if err != nil || len(findings) != 1 || findings[0].Symbol != "F@3" {
		t.Fatalf("diagnostic lost its source identity: %+v, %v", findings, err)
	}
	issue.Pos.Filename = filepath.Join(root, "value.go")
	if _, err = funlenFindings(root, []funlenIssue{issue}, sources); err != nil {
		t.Fatal(err)
	}
}

func TestGoFunctionDiagnosticRejectsUnboundOrForeignOutput(t *testing.T) {
	root := t.TempDir()
	valid := funlenIssue{FromLinter: "funlen", Text: "length violation"}
	valid.Pos.Filename, valid.Pos.Line = "value.go", 3
	sources := []policy.Source{{Path: "value.go", Functions: []policy.Function{{Name: "F", Line: 3}}}}
	for _, mutation := range []func(*funlenIssue){
		func(issue *funlenIssue) { issue.Pos.Filename = "../outside.go" },
		func(issue *funlenIssue) { issue.Pos.Filename = "missing.go" },
		func(issue *funlenIssue) { issue.Pos.Line = 9 },
		func(issue *funlenIssue) { issue.FromLinter = "unrelated" },
		func(issue *funlenIssue) { issue.Text = "" },
	} {
		issue := valid
		mutation(&issue)
		if _, err := funlenFindings(root, []funlenIssue{issue}, sources); err == nil {
			t.Fatalf("unbound diagnostic accepted: %+v", issue)
		}
	}
}
