// Package provider contains the consumer-owned provider contract and catalog.
// Concrete adapters live in child packages and are registered by composition
// roots; this package never imports an adapter implementation.
package provider

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/provider/contract"
	providerruntime "github.com/hishamkaram/delegation-layer/internal/provider/runtime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

var (
	// ErrProfileUnavailable identifies a provider mode or runtime that cannot
	// be prepared for this request.
	ErrProfileUnavailable = contract.ErrProfileUnavailable
	// ErrDuplicateProvider identifies a catalog containing two registrations for
	// the same provider identifier.
	ErrDuplicateProvider = errors.New("duplicate provider registration")
	// ErrDuplicatePredicate identifies a catalog containing one exact predicate
	// reference more than once.
	ErrDuplicatePredicate = errors.New("duplicate provider predicate reference")
	// ErrInvalidDescriptor identifies malformed immutable discovery metadata.
	ErrInvalidDescriptor = errors.New("invalid provider descriptor")
	// ErrUnsupportedOption identifies a request option not supported by the
	// selected provider.
	ErrUnsupportedOption = errors.New("unsupported provider option")
	// ErrAuthenticationBlocked identifies an unavailable native login. It is a
	// neutral prerequisite result, separate from provider incompatibility.
	ErrAuthenticationBlocked = errors.New("provider authentication unavailable")
)

// The pure discovery vocabulary lives in provider/contract; these names are
// the same types and constants.
const (
	OptionInteractive   = contract.OptionInteractive
	OptionContinuation  = contract.OptionContinuation
	OptionEffort        = contract.OptionEffort
	OptionModel         = contract.OptionModel
	OptionNativeTimeout = contract.OptionNativeTimeout
	OptionReviewTarget  = contract.OptionReviewTarget
)

type (
	ContinuationMode    = contract.ContinuationMode
	ReadOnlyMechanism   = contract.ReadOnlyMechanism
	ReadOnlyContainment = contract.ReadOnlyContainment
	ReadOnlyCapability  = contract.ReadOnlyCapability
	RuntimeCapability   = contract.RuntimeCapability
	Description         = contract.Description
)

const (
	ContinuationNative      = contract.ContinuationNative
	ContinuationCheckpoint  = contract.ContinuationCheckpoint
	ContinuationUnsupported = contract.ContinuationUnsupported

	ReadOnlyNativePlan    = contract.ReadOnlyNativePlan
	ReadOnlyNativeSandbox = contract.ReadOnlyNativeSandbox
	ReadOnlyToolAllowlist = contract.ReadOnlyToolAllowlist

	ReadOnlyProviderOwned = contract.ReadOnlyProviderOwned
)

// PreparedProfile is the immutable, provider-produced execution input. It
// carries no process, pipe, stop, capture, sealing, collection, or publication
// authority. The app runner owns those lifetimes after preparation returns.
type PreparedProfile struct {
	Interaction     func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.InteractionProtocol, error)
	Plan            execution.Plan
	ObservedVersion string
	// RuntimePreflightSHA256 names the immutable capability contract still to
	// be checked. It is empty only after finalization from observed facts.
	RuntimePreflightSHA256 string
	Effective              task.EffectiveConfig
	Identity               func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.IdentityObserver, error)
	// WritableRoots includes provider runtime, temporary, and cache roots.
	// Native resolvers must include this canonical list in Effective.Digest.
	WritableRoots []string
}

// ValidateStatePlacement rejects state that overlaps any provider-writable
// tree. It runs before admission and again in the runner after policy refresh.
func (p PreparedProfile) ValidateStatePlacement(root string) error {
	roots := append([]string{p.Plan.Directory}, p.WritableRoots...)
	return providerruntime.ValidateStatePlacement(root, roots)
}

// Validate checks the provider-produced profile against the requested task.
// It validates shape and identity only; the caller remains responsible for
// process admission and launch.
func (p PreparedProfile) Validate(request task.TaskRecord) error {
	if p.RuntimePreflightSHA256 != "" || p.Plan.RuntimePreflightSHA256 != "" || p.ObservedVersion == "" {
		return errors.New("profile requires a completed runtime preflight")
	}
	return p.validateShape(request)
}

// ValidateAdmission validates static admission without claiming an observed
// version. Finalized profiles retain the strict Validate contract.
func (p PreparedProfile) ValidateAdmission(request task.TaskRecord) error {
	if p.RuntimePreflightSHA256 == "" {
		return p.Validate(request)
	}
	if task.ValidateSHA256(p.RuntimePreflightSHA256) != nil || p.ObservedVersion != "" ||
		p.Plan.ProviderRuntime != (task.ProviderRuntimeIdentity{}) || task.ValidateSHA256(p.Plan.ExecutableSHA256) != nil ||
		(p.Plan.RuntimePreflightSHA256 != "" && p.Plan.RuntimePreflightSHA256 != p.RuntimePreflightSHA256) || p.Plan.Environment == nil {
		return task.ErrIdentityMismatch
	}
	return p.validateShape(request)
}

func (p PreparedProfile) validateShape(request task.TaskRecord) error {
	if request.RequestedConfig.Interactive != (p.Interaction != nil) {
		return ErrUnsupportedOption
	}
	if err := validatePlanBindings(p.Plan); err != nil {
		return err
	}
	if !filepath.IsAbs(p.Plan.Executable) {
		return errors.New("profile must resolve an absolute executable")
	}
	if p.Plan.Directory != request.CanonicalCwd || p.Plan.Predicate.Adapter != request.Provider || p.Plan.Predicate.Mode != request.Mode {
		return task.ErrIdentityMismatch
	}
	if err := task.ValidatePredicateRef(p.Plan.Predicate); err != nil {
		return err
	}
	if p.Effective.Containment == "" || p.Effective.Approval == "" {
		return errors.New("profile must declare effective containment and approval")
	}
	if err := task.ValidateEffectiveConfig(p.Effective); err != nil {
		return err
	}
	if p.Effective.Policy != nil && (p.Effective.Policy.Workspace != request.CanonicalCwd || !slices.Equal(p.WritableRoots, p.Effective.Policy.WritableRoots)) {
		return task.ErrIdentityMismatch
	}
	return nil
}

// Matches checks a freshly prepared profile against immutable admitted
// metadata. It retains the original executable, version, and effective
// configuration identity checks for callers that have not performed a fresh
// runtime capability probe.
func (p PreparedProfile) Matches(request task.TaskRecord, meta task.MetaRecord) error {
	return p.matches(request, meta, false)
}

// MatchesWithRuntimeRefresh checks a profile after the caller has performed
// the current supervised runtime capability probe. Provider executable path,
// reported version, and runtime-derived policy identity may change; the task
// policy, workspace, predicate, writable roots, and artifact declarations
// remain immutable.
func (p PreparedProfile) MatchesWithRuntimeRefresh(request task.TaskRecord, meta task.MetaRecord) error {
	if p.RuntimePreflightSHA256 != "" {
		return task.ErrIdentityMismatch
	}
	return p.matches(request, meta, true)
}

// MatchesPendingRuntimeRefresh reconstructs a pending task after executable
// replacement while preserving its required capability and immutable policy.
// It cannot satisfy launch validation; this task still needs fresh preflight.
func (p PreparedProfile) MatchesPendingRuntimeRefresh(request task.TaskRecord, meta task.MetaRecord) error {
	if p.RuntimePreflightSHA256 == "" || p.RuntimePreflightSHA256 != meta.RuntimePreflightSHA256 {
		return task.ErrIdentityMismatch
	}
	return p.matches(request, meta, true)
}

func (p PreparedProfile) matches(request task.TaskRecord, meta task.MetaRecord, allowRuntimeRefresh bool) error {
	validate := p.Validate
	if p.RuntimePreflightSHA256 != "" || (!allowRuntimeRefresh && meta.RuntimePreflightSHA256 != "") {
		if p.RuntimePreflightSHA256 != meta.RuntimePreflightSHA256 {
			return task.ErrIdentityMismatch
		}
		validate = p.ValidateAdmission
	}
	if err := validate(request); err != nil {
		return err
	}
	if allowRuntimeRefresh && meta.Environment == nil && !meta.EnvironmentRecorded {
		// A legacy record without its original launch environment cannot prove
		// that a refreshed runtime was inspected under the same inputs. Keep
		// legacy replay available through Matches, but fail closed at the fresh
		// runtime boundary. A fresh record may intentionally have an empty
		// environment; EnvironmentRecorded preserves that fact after JSON
		// omitempty removes the empty slice.
		return task.ErrIdentityMismatch
	}
	inputs, err := task.NormalizeInputFiles(p.Plan.InputFiles)
	if err != nil {
		return err
	}
	outputs, err := task.NormalizeOutputArtifacts(p.Plan.OutputArtifacts)
	if err != nil {
		return err
	}
	if !profileMatchesAdmittedContract(p, meta, inputs, outputs, allowRuntimeRefresh) {
		return task.ErrIdentityMismatch
	}
	return nil
}

func profileMatchesAdmittedContract(profile PreparedProfile, meta task.MetaRecord, inputs []task.InputFile, outputs []task.OutputArtifact, allowRuntimeRefresh bool) bool {
	if !allowRuntimeRefresh && (profile.Plan.Executable != meta.ProviderExecutable || profile.ObservedVersion != meta.ProviderVersion) {
		return false
	}
	effectiveMatches := effectiveConfigMatches(profile, meta)
	if allowRuntimeRefresh {
		effectiveMatches = effectiveConfigMatchesAfterRuntimeRefresh(profile, meta)
	}
	return environmentMatches(profile, meta) && effectiveMatches &&
		profile.Plan.Predicate.Equal(meta.Predicate) && task.CompareInputFiles(inputs, meta.InputFiles) &&
		task.CompareOutputArtifacts(outputs, meta.OutputArtifacts) && profile.Plan.OutputWriterContract == meta.OutputWriterContract
}

func environmentMatches(profile PreparedProfile, meta task.MetaRecord) bool {
	if meta.Environment == nil && !meta.EnvironmentRecorded {
		return true
	}
	return slices.Equal(profile.Plan.Environment, meta.Environment)
}

func effectiveConfigMatches(profile PreparedProfile, meta task.MetaRecord) bool {
	if meta.Environment == nil && !meta.EnvironmentRecorded {
		if meta.EffectiveConfig.Policy == nil || profile.Effective.Policy == nil {
			return task.CompareEffectiveConfigs(profile.Effective, meta.EffectiveConfig)
		}
		// The original record did not retain the launch environment, so its
		// environment-derived policy digest cannot be reconstructed. Persisted
		// writable roots remain authoritative and are compared here so a changed
		// runtime or cache placement cannot broaden the launch policy.
		return profile.Effective.Containment == meta.Containment && profile.Effective.Approval == meta.Approval &&
			profile.Effective.Policy.ProfileRevision == meta.EffectiveConfig.Policy.ProfileRevision &&
			profile.Effective.Policy.RuntimeSHA256 == meta.EffectiveConfig.Policy.RuntimeSHA256 &&
			profile.Effective.Policy.Workspace == meta.EffectiveConfig.Policy.Workspace &&
			slices.Equal(profile.Effective.Policy.WritableRoots, meta.EffectiveConfig.Policy.WritableRoots) &&
			slices.Equal(profile.Effective.Policy.Sources, meta.EffectiveConfig.Policy.Sources)
	}
	return task.CompareEffectiveConfigs(profile.Effective, meta.EffectiveConfig)
}

func effectiveConfigMatchesAfterRuntimeRefresh(profile PreparedProfile, meta task.MetaRecord) bool {
	return task.CompareEffectiveConfigContract(profile.Effective, meta.EffectiveConfig)
}

func validatePlanBindings(plan execution.Plan) error {
	if err := task.ValidateInputOutputBindings(plan.InputFiles, plan.OutputArtifacts); err != nil {
		return err
	}
	if err := task.ValidateOutputArtifacts(plan.OutputArtifacts, plan.OutputWriterContract); err != nil {
		return err
	}
	for _, file := range plan.InputFiles {
		if file.ArgumentIndex >= len(plan.Arguments) {
			return fmt.Errorf("input file %q argument index %d is outside argv", file.Name, file.ArgumentIndex)
		}
		if plan.Arguments[file.ArgumentIndex] != "" {
			return fmt.Errorf("input file %q argument slot %d must be empty", file.Name, file.ArgumentIndex)
		}
	}
	for _, artifact := range plan.OutputArtifacts {
		if artifact.ArgumentIndex >= len(plan.Arguments) {
			return fmt.Errorf("output artifact %q argument index %d is outside argv", artifact.Name, artifact.ArgumentIndex)
		}
		if plan.Arguments[artifact.ArgumentIndex] != "" {
			return fmt.Errorf("output artifact %q argument slot %d must be empty", artifact.Name, artifact.ArgumentIndex)
		}
	}
	return nil
}

// PrepareProfile is the narrow preparation function accepted by the app and
// by hermetic acceptance compositions.
type PrepareProfile func(task.TaskRecord) (PreparedProfile, error)

// Registration is one explicit catalog entry. A nil Prepare function is
// valid for historical-only predicate registrations and never grants launch
// authority.
type Registration struct {
	// Models describes optional advisory discovery. It grants no task admission.
	Models               func() ModelDiscoveryDefinition
	Description          Description
	Prepare              PrepareCandidate
	PrepareExisting      PrepareExistingCandidate
	ValidateContinuation ContinuationValidator
	Interpreters         []predicate.Interpreter
}

// ContinuationValidator checks provider-owned compatibility between a
// predecessor request and its proposed successor. predecessorMeta is the
// predecessor's immutable prepared record, including the sealed predicate
// version that can affect provider continuation semantics. The catalog
// performs the shared provider and permission identity checks before invoking
// it.
type ContinuationValidator func(predecessor task.TaskRecord, predecessorMeta task.MetaRecord, successor task.TaskRecord) error
