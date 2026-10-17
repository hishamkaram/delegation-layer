package maintainability

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

func TestDiagramAuthoringSourcesKeepDocumentationBudget(t *testing.T) {
	cases := []struct {
		path string
		kind string
		max  int
	}{
		{path: "docs/architecture.md", kind: "documentation", max: 1800},
		{path: "diagrams/components.architecture.json", kind: "documentation", max: 1800},
		{path: "diagrams/renamed.sequence.json", kind: "documentation", max: 1800},
		{path: "diagrams/config.json", kind: "code", max: 800},
		{path: "internal/layout.architecture.json", kind: "code", max: 800},
	}
	manifest := policy.Manifest{Limits: policy.Limits{Code: 800, Tests: 1000, Documentation: 1800, Function: 80}}
	for _, test := range cases {
		t.Run(test.path, func(t *testing.T) {
			kind := sourceKind(test.path)
			if kind != test.kind {
				t.Fatalf("source kind = %q, want %q", kind, test.kind)
			}
			source := policy.Source{Path: test.path, Kind: kind, Lines: test.max}
			if findings := policy.Check(manifest, []policy.Source{source}); len(findings) != 0 {
				t.Fatalf("exact budget rejected: %+v", findings)
			}
			source.Lines++
			findings := policy.Check(manifest, []policy.Source{source})
			if len(findings) != 1 || findings[0].Kind != "size" || findings[0].Limit != test.max {
				t.Fatalf("over-budget source not rejected: %+v", findings)
			}
		})
	}
}
