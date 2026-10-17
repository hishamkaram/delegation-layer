package maintainability

import (
	"strconv"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

func TestPythonPositionalCallThreshold(t *testing.T) {
	for _, count := range []int{7, 8, 25} {
		arguments := make([]string, count)
		named := make([]string, count)
		for index := range arguments {
			arguments[index] = "value" + strconv.Itoa(index)
			named[index] = arguments[index] + "=" + arguments[index]
		}
		findings := pythonCallFindings(t, "fixture.py", "target("+strings.Join(arguments, ", ")+")\n")
		if count < 8 {
			if len(findings) != 0 {
				t.Errorf("valid positional call rejected: count=%d: %+v", count, findings)
			}
		} else if len(findings) != 1 || findings[0].Kind != "positional-call" || findings[0].Actual != count || findings[0].Limit != 7 || findings[0].Symbol != "target" || findings[0].Line != 1 || findings[0].Path != "fixture.py" {
			t.Errorf("large call not reported: count=%d: %+v", count, findings)
		}
		if namedFindings := pythonCallFindings(t, "fixture.py", "target("+strings.Join(named, ", ")+")\n"); len(namedFindings) != 0 {
			t.Errorf("valid named call rejected: count=%d: %+v", count, namedFindings)
		}
	}
}

// Exercise the same collector-to-inventory merge used by Audit, not just decoding.
func pythonCallFindings(t *testing.T, path, text string) []policy.Finding {
	t.Helper()
	root := collectorRoot(t)
	writeCollectorFixture(t, root, path, text)
	sources := []policy.Source{{Path: path, Kind: "test"}}
	paths := []string{path}
	facts, err := languageFacts(root, "python", paths)
	if err != nil {
		t.Fatal(err)
	}
	if err := mergeFacts("python", paths, facts, sources); err != nil {
		t.Fatal(err)
	}
	var imports []string
	for _, dependency := range sources[0].Imports {
		imports = append(imports, dependency.Target)
	}
	manifest := policy.Manifest{Limits: policy.Limits{Function: 80}, Modules: []policy.Module{{Name: sources[0].Module, Owner: "fixture", Layer: "test", Imports: imports}}}
	return policy.Check(manifest, sources)
}

func TestPythonLargeDeclarationCalledWithKeywordsIsValid(t *testing.T) {
	parameters := make([]string, 20)
	arguments := make([]string, 20)
	for index := range parameters {
		parameters[index] = "value" + strconv.Itoa(index)
		arguments[index] = parameters[index] + "=1"
	}
	source := "def target(" + strings.Join(parameters, ", ") + "):\n    return 1\ntarget(" + strings.Join(arguments, ", ") + ")\n"
	if findings := pythonCallFindings(t, "fixture.py", source); len(findings) != 0 {
		t.Fatalf("declaration shape was treated as an unnamed call: %+v", findings)
	}
}

func TestPythonPositionalCallsAcrossASTContexts(t *testing.T) {
	arguments := "a, b, c, d, e, f, g, h"
	cases := []struct {
		name, source, callee string
		line                 int
	}{
		{"nested", "outer(inner(" + arguments + "))", "inner", 1},
		{"lambda_body", "run = lambda: target(" + arguments + ")", "target", 1},
		{"lambda_callee", "(lambda *values: values)(" + arguments + ")", "lambda *values: values", 1},
		{"comprehension", "[target(" + arguments + ") for value in values]", "target", 1},
		{"decorator", "@target(" + arguments + ")\ndef decorated():\n    pass", "target", 1},
		{"default", "def later(value=target(" + arguments + ")):\n    pass", "target", 1},
		{"classmethod", "class Record:\n    @classmethod\n    def make(cls):\n        return cls(" + arguments + ")", "cls", 4},
		{"partial", "import functools\nfunctools.partial(target, a, b, c, d, e, f, g)", "functools.partial", 2},
		{"expression_callee", "factories[\"record\"](" + arguments + ")", "factories['record']", 1},
		{"dataclass", "from dataclasses import dataclass\n@dataclass(frozen=True)\nclass Record:\n    a: int; b: int; c: int; d: int\n    e: int; f: int; g: int; h: int\nRecord(" + arguments + ")", "Record", 6},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			path := "scripts/acceptance_tests/fixture.py"
			findings := pythonCallFindings(t, path, test.source)
			if len(findings) != 1 || findings[0].Kind != "positional-call" || findings[0].Symbol != test.callee || findings[0].Line != test.line || findings[0].Path != path || findings[0].Actual != 8 {
				t.Fatalf("call context lost: %+v", findings)
			}
		})
	}
}

func TestPythonPositionalCallsExpandLiteralStarsRecursively(t *testing.T) {
	sources := []string{
		"target(*(a, b, c, d, e, f, g, h))",
		"target(a, *[b, c, d, e, f, g, h])",
		"target(*(a, *[b, *(c, d), e], f, g, h))",
		"target(*[a, *(b, *[c, d, e]), f, g], h)",
		"target(*unknown, a, b, c, d, e, f, g, h)",
		"target(*(a, *unknown, b, c, d, e, f, g, h))",
	}
	for _, source := range sources {
		findings := pythonCallFindings(t, "fixture.py", source)
		if len(findings) != 1 || findings[0].Kind != "positional-call" || findings[0].Actual != 8 {
			t.Fatalf("known positional values were lost: %s: %+v", source, findings)
		}
	}
}

func TestPythonPositionalCallsKeepNamedValuesAndBoundedForwarding(t *testing.T) {
	sources := []string{
		"target(a, b, c, d, e, f, g, h=h)",
		"target(*args, **kwargs)", // Dynamic starred values have unknown arity, not an inferred zero.
		"target(*(a, b, *unknown, c, d, e, f, g))",
		"target([a, b, c, d, e, f, g, h])",
		"target((a, b, c, d, e, f, g, h))",
		"target(*(a, *[b, (c, d, e, f, g, h)]))", // An unstarred nested tuple is one value.
		"target(*())",
	}
	for _, source := range sources {
		if findings := pythonCallFindings(t, "fixture.py", source); len(findings) != 0 {
			t.Fatalf("valid or unresolved call rejected: %s: %+v", source, findings)
		}
	}
}

func TestPythonFactsRejectMalformedSyntax(t *testing.T) {
	root := collectorRoot(t)
	writeCollectorFixture(t, root, "fixture.py", "target(\n")
	if _, err := languageFacts(root, "python", []string{"fixture.py"}); err == nil {
		t.Fatal("malformed Python source accepted")
	}
}
