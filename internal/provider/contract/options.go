// Package contract holds the pure provider discovery vocabulary and the state
// placement rule. It performs no filesystem, process, or clock access: callers
// supply observed facts and receive a decision.
package contract

// Request option names a provider may advertise as supported or required.
const (
	OptionInteractive   = "interactive"
	OptionContinuation  = "continuation"
	OptionEffort        = "effort"
	OptionModel         = "model"
	OptionNativeTimeout = "native-timeout"
	OptionReviewTarget  = "review-target"
)

// ContinuationMode describes how a provider resumes a predecessor session.
// The value is discovery metadata; the adapter still proves the exact
// session identity at launch and publication time.
type ContinuationMode string

const (
	ContinuationNative      ContinuationMode = "native"
	ContinuationCheckpoint  ContinuationMode = "checkpoint"
	ContinuationUnsupported ContinuationMode = "unsupported"
)

// ReadOnlyMechanism identifies the native behavior selected for a provider's
// read-only request. It describes the adapter contract; it is not a delegate
// owned sandbox guarantee.
type ReadOnlyMechanism string

const (
	ReadOnlyNativePlan    ReadOnlyMechanism = "native-plan"
	ReadOnlyNativeSandbox ReadOnlyMechanism = "native-sandbox"
	ReadOnlyToolAllowlist ReadOnlyMechanism = "tool-allowlist"
)

// ReadOnlyContainment identifies who owns the effective boundary around the
// provider's configuration, extensions, and native tools.
type ReadOnlyContainment string

const ReadOnlyProviderOwned ReadOnlyContainment = "provider-owned"

// ReadOnlyCapability is static metadata for a provider's read-only mode.
// Supported is false and the other fields are empty when read-only is not
// advertised.
type ReadOnlyCapability struct {
	Supported   bool                `json:"supported"`
	Mechanism   ReadOnlyMechanism   `json:"mechanism,omitempty"`
	Containment ReadOnlyContainment `json:"containment,omitempty"`
}
