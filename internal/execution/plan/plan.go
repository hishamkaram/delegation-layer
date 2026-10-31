// Package plan holds the immutable launch plan a trusted compiled adapter hands
// to the execution runner. It is plain data: it owns no process, pipe, clock, or
// filesystem authority.
package plan

import "github.com/hishamkaram/delegation-layer/internal/task"

// Plan is constructed by a trusted compiled adapter, never from public raw argv.
type Plan struct {
	Executable string
	// ExecutableSHA256 is an optional historical provider digest. It is never
	// verified at launch; the installed executable at Executable runs directly.
	// When ProviderRuntime is present, both must carry the same value.
	ExecutableSHA256 string
	// RuntimePreflightSHA256 binds a pending plan to the immutable admission
	// contract. Only a successful PreflightPlan may clear it before Start.
	// While nonempty, ProviderRuntime must be empty.
	RuntimePreflightSHA256 string
	// ProviderRuntime is the identity observed for this plan. It is populated
	// by the app after provider finalization and recorded at start/seal so a
	// runtime upgrade remains reconstructable without mutating meta.json.
	ProviderRuntime      task.ProviderRuntimeIdentity
	Arguments            []string
	Directory            string
	Environment          []string
	Predicate            task.PredicateRef
	InputFiles           []task.InputFile
	OutputArtifacts      []task.OutputArtifact
	OutputWriterContract string
}
