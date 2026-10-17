package contract

import provider "github.com/hishamkaram/delegation-layer/internal/provider/contract"

// FactsLimit selects the persisted inspection contract's evidence bound.
func FactsLimit(revision string) int {
	switch revision {
	case provider.ModelsRevision:
		return provider.MaxModelFactsBytes
	case provider.RuntimeInspectionRevision:
		// Runtime envelopes contain bounded version text and capability metadata,
		// so use the complete control-record budget rather than the native
		// projection budget.
		return MaxRuntimeFacts
	default:
		return MaxProjectedFacts
	}
}
