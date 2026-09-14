package app

import (
	"errors"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/provider/antigravity"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestProductionCollectionRegistryRetainsExactContracts(t *testing.T) {
	registry := (Dependencies{}).storeDependencies().registry()
	for _, reference := range []task.PredicateRef{task.FixturePredicateRef(), antigravity.NewPrintInterpreter().Reference(), antigravity.NewCurrentPrintInterpreter().Reference()} {
		interpreter, err := registry.Resolve(reference)
		if err != nil {
			t.Fatalf("resolve %s: %v", reference.Adapter, err)
		}
		if !interpreter.Reference().Equal(reference) {
			t.Fatalf("resolved a different contract for %s", reference.Adapter)
		}
		changed := reference
		changed.SHA256 = task.ComputeSHA256([]byte("unregistered contract"))
		if _, err = registry.Resolve(changed); !errors.Is(err, task.ErrIncompatiblePredicate) {
			t.Fatalf("changed digest was accepted for %s: %v", reference.Adapter, err)
		}
	}
}
