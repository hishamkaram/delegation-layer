package maintainability

import (
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

func TestRegistryLayerVocabularyIsClosed(t *testing.T) {
	for _, layer := range []string{"pure", "runtime", "application", "test", "tool"} {
		if err := validateModules([]policy.Module{{Name: "fixture", Owner: "fixture", Layer: layer}}); err != nil {
			t.Fatalf("known layer %q rejected: %v", layer, err)
		}
	}
	for _, layer := range []string{"", "Pure", "unknown", "runtime-application"} {
		if err := validateModules([]policy.Module{{Name: "fixture", Owner: "fixture", Layer: layer}}); err == nil {
			t.Fatalf("unrecognized layer %q accepted", layer)
		}
	}
}
