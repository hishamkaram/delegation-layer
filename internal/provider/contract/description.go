package contract

import "github.com/hishamkaram/delegation-layer/internal/task"

// RuntimeCapability describes the command-level checks performed when a
// launch profile is prepared. VersionArgs selects the provider's version
// command and defaults to ["--version"] when empty; HelpArgs are prepended
// before --help (for example, Codex uses ["exec"]); RequiredFlags are the
// flags the adapter puts in its argv. These are requirements, not a release
// manifest.
type RuntimeCapability struct {
	VersionArgs   []string `json:"version_args,omitempty"`
	HelpArgs      []string `json:"help_args,omitempty"`
	RequiredFlags []string `json:"required_flags"`
}

// Description is the bounded metadata exposed by the providers command.
// Discoverable controls whether this registration appears in that response.
type Description struct {
	Interaction      task.InteractionCapabilities `json:"interaction"`
	ID               string                       `json:"id"`
	SupportedModes   []string                     `json:"supported_modes"`
	SupportedOptions []string                     `json:"supported_options"`
	RequiredOptions  []string                     `json:"required_options,omitempty"`
	Continuation     ContinuationMode             `json:"continuation"`
	ReadOnly         ReadOnlyCapability           `json:"read_only"`
	Runtime          RuntimeCapability            `json:"runtime"`
	Discoverable     bool                         `json:"-"`
}
