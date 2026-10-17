package maintainability

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

const parserContractSource = `from . import values
from datetime import datetime as Clock
from uuid import uuid4 as fresh
async def combine(a, /, b=None, *, named=True):
    return Clock.fromisoformat(a)
Clock.now()
fresh
raise RuntimeError('never evaluate')
`

// The call-site contract retains existing observations and replaces declaration counts.
const parserContractJSON = `{"path": "scripts/fixture/__init__.py", "module": "py:fixture", "imports": [{"target": "py:fixture.values", "line": 1}, {"target": "py:datetime", "line": 2}, {"target": "py:uuid", "line": 3}], "functions": [{"name": "combine", "line": 4, "length": 2}], "calls": [{"name": "Clock.now", "line": 6, "positional_arguments": 0}, {"name": "RuntimeError", "line": 8, "positional_arguments": 1}, {"name": "Clock.fromisoformat", "line": 5, "positional_arguments": 1}], "effects": [{"name": "datetime.datetime.now", "line": 6}, {"name": "uuid.uuid4", "line": 7}]}`

func TestPythonPureParserFactContract(t *testing.T) {
	command := exec.Command("python3", "-B", "-c", `import json, sys
from scripts.maintainability.python_ast import parse_source_facts
json.dump(parse_source_facts("scripts/fixture/__init__.py", sys.stdin.read(), {"fixture", "fixture.values"}), sys.stdout)
`)
	command.Dir = filepath.Join("..", "..")
	command.Stdin = strings.NewReader(parserContractSource)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("pure parser boundary unavailable: %v: %s", err, output)
	}
	if string(output) != parserContractJSON {
		t.Fatalf("unexpected pure parser facts:\n%s\nwant:\n%s", output, parserContractJSON)
	}
}

func TestPythonCollectorMatchesPureParserFactContract(t *testing.T) {
	root := collectorRoot(t)
	paths := []string{"scripts/fixture/__init__.py", "scripts/fixture/values.py"}
	writeCollectorFixture(t, root, paths[0], parserContractSource)
	writeCollectorFixture(t, root, paths[1], "raise RuntimeError('never evaluate')\n")
	command := exec.Command("python3", "-B", "scripts/maintainability/python_facts.py")
	command.Dir = root
	command.Stdin = strings.NewReader(`["scripts/fixture/__init__.py", "scripts/fixture/values.py"]`)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("collector failed: %v: %s", err, output)
	}
	var facts []json.RawMessage
	if err := json.Unmarshal(output, &facts); err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 || string(facts[0]) != parserContractJSON {
		t.Fatalf("CLI facts differ from the pure parser contract: %s", output)
	}
}

func TestPythonParserHasOnlyItsPureASTDependency(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	facts, err := languageFacts(root, "python", []string{"scripts/maintainability/python_ast.py"})
	if err != nil {
		t.Fatal(err)
	}
	manifest := policy.Manifest{
		Limits:  policy.Limits{Function: 80},
		Modules: []policy.Module{{Name: "py:maintainability.python_ast", Owner: "parser", Layer: "pure", Imports: []string{"py:ast"}}},
	}
	if len(facts) != 1 {
		t.Fatalf("missing pure parser facts: %+v", facts)
	}
	if findings := policy.Check(manifest, facts); len(findings) != 0 {
		t.Fatalf("parser crossed its pure boundary: %+v", findings)
	}
}
