package journal

import (
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestDiscoveryFactsDoNotWidenHistoricalProjectionLimit(t *testing.T) {
	data := []byte(`{"models":"` + strings.Repeat("a", maxProjectedFacts) + `"}`)
	if _, err := canonicalProjectionFacts(data); err == nil {
		t.Fatal("historical limit widened")
	}
	op := &Operation{request: RequestRecord{Binding: Binding{DefinitionRevision: provider.ModelsRevision}}}
	if _, err := canonicalFactsWithin(data, op.factsLimit()); err != nil {
		t.Fatal(err)
	}
	op.request.Binding.DefinitionRevision = "legacy"
	if op.factsLimit() != maxProjectedFacts {
		t.Fatal("legacy journal limit changed")
	}
	op.request.Binding.DefinitionRevision = provider.RuntimeInspectionRevision
	if op.factsLimit() != maxRuntimeFacts {
		t.Fatal("runtime journal did not reserve space for capability metadata")
	}
	if _, err := canonicalFactsWithin([]byte(strings.Repeat("a", task.MaxControlRecordSize)), task.MaxControlRecordSize/2); err == nil {
		t.Fatal("oversized model facts accepted")
	}
}
