package policy

import "testing"

func TestPositionalCallPolicyOnlyAppliesToPython(t *testing.T) {
	for _, module := range []string{"py:fixture", "go:fixture", "js:fixture"} {
		source := Source{Path: "fixture", Module: module, Kind: "test", Calls: []Call{{Name: "target", Line: 3, PositionalArguments: 8}}}
		manifest := Manifest{Modules: []Module{{Name: module, Owner: "fixture", Layer: "test"}}}
		findings := Check(manifest, []Source{source})
		want := 0
		if module == "py:fixture" {
			want = 1
		}
		if len(findings) != want {
			t.Fatalf("wrong language scope for %s: %+v", module, findings)
		}
	}
}

func TestPositionalCallFindingFieldsAndStableKey(t *testing.T) {
	source := Source{
		Path: "scripts/acceptance_tests/fixture.py", Module: "py:fixture", Kind: "test",
		Calls: []Call{{Name: "factories['record']", Line: 12, PositionalArguments: 25}},
	}
	manifest := Manifest{Modules: []Module{{Name: source.Module, Owner: "fixture", Layer: "test"}}}
	findings := Check(manifest, []Source{source})
	want := Finding{
		Path: source.Path, Line: 12, Kind: "positional-call", Symbol: "factories['record']",
		Detail: "large Python call requires named arguments", Actual: 25, Limit: 7,
	}
	if len(findings) != 1 || findings[0] != want {
		t.Fatalf("incomplete call diagnostic: %+v", findings)
	}
	key := "scripts/acceptance_tests/fixture.py:positional-call:factories['record']:large Python call requires named arguments"
	if actual := findingKey(findings[0]); actual != key {
		t.Fatalf("unstable finding key: %q, want %q", actual, key)
	}
}
