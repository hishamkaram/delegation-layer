package plan

import (
	"encoding/json"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestPlanSerializesWithUnchangedFieldNames(t *testing.T) {
	value := Plan{
		Executable:             "/bin/provider",
		ExecutableSHA256:       "exe",
		RuntimePreflightSHA256: "pending",
		ProviderRuntime:        task.ProviderRuntimeIdentity{},
		Arguments:              []string{"--flag", ""},
		Directory:              "/work",
		Environment:            []string{"A=B"},
		InputFiles:             []task.InputFile{{Name: "in", ArgumentIndex: 1}},
		OutputArtifacts:        []task.OutputArtifact{{Name: "out", ArgumentIndex: 0}},
		OutputWriterContract:   task.OutputWriterProcessExitEOF,
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	want := []string{
		"Executable", "ExecutableSHA256", "RuntimePreflightSHA256", "ProviderRuntime",
		"Arguments", "Directory", "Environment", "Predicate", "InputFiles",
		"OutputArtifacts", "OutputWriterContract",
	}
	if len(fields) != len(want) {
		t.Fatalf("encoded %d fields, want %d: %s", len(fields), len(want), encoded)
	}
	for _, name := range want {
		if _, ok := fields[name]; !ok {
			t.Errorf("encoded plan lacks field %q: %s", name, encoded)
		}
	}
}
