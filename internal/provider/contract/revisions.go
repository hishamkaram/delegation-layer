// Package contract owns immutable provider facts and pure decisions.
package contract

// RuntimeInspectionRevision identifies capability semantics, never a provider release.
const RuntimeInspectionRevision = "runtime-capability-v1"

// ModelsRevision identifies the provider model-discovery contract.
const ModelsRevision = "model-discovery-v1"

// MaxModelFactsBytes bounds one sanitized model catalog.
const MaxModelFactsBytes = 512 << 10
