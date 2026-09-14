package app

import (
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/provider/antigravity"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// NativePredicates contains only compiled interpretation contracts. Resolving
// one never launches a provider or certifies its current permission profile.
// Retain the original fixture contract so adding an adapter cannot invalidate
// collection of previously sealed evidence.
func NativePredicates() predicate.Registry {
	fixture, err := predicate.Default().Resolve(task.FixturePredicateRef())
	if err != nil {
		panic(err) // The immutable built-in registry must contain its own reference.
	}
	registry, err := predicate.New(fixture, antigravity.NewPrintInterpreter(), antigravity.NewCurrentPrintInterpreter())
	if err != nil {
		panic(err) // Compiled contracts must have distinct, valid references.
	}
	return registry
}
