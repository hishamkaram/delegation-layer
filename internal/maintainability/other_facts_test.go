package maintainability

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

func collectorRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"python_ast.py", "python_facts.py", "javascript-facts.mjs"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "scripts", "maintainability", name))
		if err != nil {
			t.Fatal(err)
		}
		writeCollectorFixture(t, root, "scripts/maintainability/"+name, string(data))
	}
	return root
}

func writeCollectorFixture(t *testing.T, root, name, source string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPythonFactsResolvePackagesWithoutImportingThem(t *testing.T) {
	root := collectorRoot(t)
	writeCollectorFixture(t, root, "pkg/__init__.py", "from . import values\nraise RuntimeError('do not import')\n")
	writeCollectorFixture(t, root, "pkg/values.py", "from .rules import validate\n")
	writeCollectorFixture(t, root, "pkg/rules.py", "def validate(value):\n    return value\n")
	facts, err := languageFacts(root, "python", []string{"pkg/__init__.py", "pkg/values.py", "pkg/rules.py"})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 3 || facts[0].Imports[0].Target != "py:pkg.values" || facts[1].Imports[0].Target != "py:pkg.rules" {
		t.Fatalf("relative imports lost: %+v", facts)
	}
}

func TestPythonSpanIncludesRawStringsAndDoesNotExemptRunPrefix(t *testing.T) {
	root := collectorRoot(t)
	for _, name := range []string{"ordinary", "runOrdinary"} {
		source := "def " + name + "():\n    value = '''\n" + strings.Repeat("}\n", 77) + "'''\n    return value\n"
		writeCollectorFixture(t, root, "fixture.py", source)
		facts, err := languageFacts(root, "python", []string{"fixture.py"})
		if err != nil {
			t.Fatal(err)
		}
		if len(facts[0].Functions) != 1 || facts[0].Functions[0].Length != 81 {
			t.Fatalf("raw-string span or name bypass: %+v", facts)
		}
	}
}

func TestJavaScriptFactsParseReexportsWithoutEvaluation(t *testing.T) {
	root := collectorRoot(t)
	writeCollectorFixture(t, root, "fixture/entry.mjs", "import x from 'node:fs'; export {x} from './values.mjs'; const decoy=\"import z from 'fake'\"; throw Error('do not evaluate');")
	facts, err := languageFacts(root, "javascript", []string{"fixture/entry.mjs"})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || len(facts[0].Imports) != 2 || facts[0].Imports[1].Target != "js:fixture/values.mjs" {
		t.Fatalf("static module facts lost: %+v", facts)
	}
	writeCollectorFixture(t, root, "fixture/broken.mjs", "export { broken")
	if _, err = languageFacts(root, "javascript", []string{"fixture/broken.mjs"}); err == nil {
		t.Fatal("malformed module passed")
	}
}

func TestMissingDuplicateAndUnidentifiedCollectorFactsFail(t *testing.T) {
	source := policy.Source{Path: "value.py"}
	cases := [][]policy.Source{
		nil,
		{{Path: "value.py"}},
		{{Path: "value.py", Module: "py:value"}, {Path: "value.py", Module: "py:value"}},
		{{Path: "different.py", Module: "py:different"}},
	}
	for _, facts := range cases {
		if err := mergeFacts("python", []string{"value.py"}, facts, []policy.Source{source}); err == nil {
			t.Fatalf("invalid collector observations accepted: %+v", facts)
		}
	}
}

func TestJavaScriptFactsDoNotInventImportPositions(t *testing.T) {
	root := collectorRoot(t)
	source := "// header\n\nthrow Error('do not evaluate');\n\nimport value from './values.mjs';\n"
	writeCollectorFixture(t, root, "fixture/entry.mjs", source)
	facts, err := languageFacts(root, "javascript", []string{"fixture/entry.mjs"})
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || len(facts[0].Imports) != 1 || facts[0].Imports[0].Target != "js:fixture/values.mjs" {
		t.Fatalf("unexpected import observations: %+v", facts)
	}
	line := facts[0].Imports[0].Line
	if line != 0 && line != 5 {
		t.Fatalf("import is on line 5: report its actual line or unknown (0), got %d", line)
	}
}
