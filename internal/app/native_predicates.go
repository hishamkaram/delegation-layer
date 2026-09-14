package app

import (
	"github.com/hishamkaram/delegation-layer/internal/predicate"
)

// NativePredicates contains only compiled interpretation contracts. Resolving
// one never launches a provider or certifies its current permission profile.
// Retain the original fixture contract so adding an adapter cannot invalidate
// collection of previously sealed evidence.
func NativePredicates() predicate.Registry {
	return NativeCatalog().Registry()
}
