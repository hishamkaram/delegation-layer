package maintainability

import "testing"

func TestPythonFactsBindRepositoryAndFlatImportsToOneModule(t *testing.T) {
	root := collectorRoot(t)
	paths := []string{"scripts/entry.py", "scripts/shared/__init__.py", "scripts/shared/values.py"}
	writeCollectorFixture(t, root, paths[0], "import shared.values\nimport scripts.shared.values\nfrom scripts.shared import values\nfrom scripts import shared\nfrom scripts.unmaintained import value\n")
	writeCollectorFixture(t, root, paths[1], "raise RuntimeError('do not import')\n")
	writeCollectorFixture(t, root, paths[2], "raise RuntimeError('do not import')\n")
	facts, err := languageFacts(root, "python", paths)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"py:shared.values", "py:shared.values", "py:shared.values", "py:shared", "py:scripts.unmaintained"}
	if len(facts) != 3 || len(facts[0].Imports) != len(want) {
		t.Fatalf("unexpected facts: %+v", facts)
	}
	for index, target := range want {
		if facts[0].Imports[index].Target != target {
			t.Fatalf("import %d: got %q, want %q", index, facts[0].Imports[index].Target, target)
		}
	}
}
