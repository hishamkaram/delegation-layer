package policy

import (
	"strings"
	"testing"
)

func TestRegisteredImportsCannotReverseLayerDirection(t *testing.T) {
	cases := []struct{ from, to string }{
		{"pure", "runtime"},
		{"runtime", "application"},
		{"runtime", "tool"},
		{"runtime", "test"},
		{"application", "tool"},
		{"application", "test"},
		{"tool", "test"},
	}
	for _, item := range cases {
		t.Run(item.from+"-to-"+item.to, func(t *testing.T) {
			manifest, sources := layerImportFixture(item.from, item.to)
			findings := Check(manifest, sources)
			if len(findings) != 1 || findings[0].Kind != "dependency" || findings[0].Path != "source.py" || findings[0].Line != 7 {
				t.Fatalf("a registered outward dependency escaped policy: %+v", findings)
			}
			if !strings.Contains(findings[0].Detail, "py:target") {
				t.Errorf("dependency diagnostic lost its actual boundary: %+v", findings)
			}
		})
	}
}

func TestLayerDirectionAllowsRuntimePortsAndOuterComposition(t *testing.T) {
	for _, item := range []struct{ from, to string }{
		{"pure", "pure"},
		{"runtime", "pure"},
		{"runtime", "runtime"},
		{"application", "runtime"},
		{"application", "application"},
		{"tool", "application"},
		{"tool", "tool"},
		{"test", "application"},
		{"test", "tool"},
		{"test", "test"},
	} {
		t.Run(item.from+"-to-"+item.to, func(t *testing.T) {
			manifest, sources := layerImportFixture(item.from, item.to)
			if findings := Check(manifest, sources); len(findings) != 0 {
				t.Fatalf("an inward or outer composition dependency was rejected: %+v", findings)
			}
		})
	}
}

func TestLayerDirectionDoesNotGrantUnknownRoles(t *testing.T) {
	for _, item := range []struct{ from, to string }{{"future", "pure"}, {"test", "future"}, {"tool", ""}} {
		if allowedLayerDependency(item.from, item.to) {
			t.Errorf("unknown role authorized: %+v", item)
		}
	}
}

func layerImportFixture(from, to string) (Manifest, []Source) {
	return Manifest{Modules: []Module{
		{Name: "py:source", Owner: "source", Layer: from, Imports: []string{"py:target"}},
		{Name: "py:target", Owner: "target", Layer: to},
	}}, []Source{
		{Path: "source.py", Module: "py:source", Imports: []Dependency{{Target: "py:target", Line: 7}}},
		{Path: "target.py", Module: "py:target"},
	}
}
