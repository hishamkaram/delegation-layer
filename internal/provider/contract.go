// Package provider contains the consumer-owned provider contract and catalog.
// Concrete adapters live in child packages and are registered by composition
// roots; this package never imports an adapter implementation.
package provider

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

var (
	// ErrProfileUnavailable identifies a provider mode or runtime that cannot
	// be prepared for this request.
	ErrProfileUnavailable = errors.New("provider permission profile is not available")
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

const (
	OptionContinuation  = "continuation"
	OptionEffort        = "effort"
	OptionModel         = "model"
	OptionNativeTimeout = "native-timeout"
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

// PreparedProfile is the immutable, provider-produced execution input. It
// carries no process, pipe, stop, capture, sealing, collection, or publication
// authority. The app runner owns those lifetimes after preparation returns.
type PreparedProfile struct {
	Plan            execution.Plan
	ObservedVersion string
	Effective       task.EffectiveConfig
	Identity        func(task.SessionExpectation, func(task.SessionIdentity) error) (execution.IdentityObserver, error)
	// WritableRoots includes provider runtime, temporary, and cache roots.
	// Native resolvers must include this canonical list in Effective.Digest.
	WritableRoots []string
}

// ValidateStatePlacement rejects state that overlaps any provider-writable
// tree. It runs before admission and again in the runner after policy refresh.
func (p PreparedProfile) ValidateStatePlacement(root string) error {
	canonicalRoot, err := config.CanonicalizePath(root)
	if err != nil || canonicalRoot != root {
		return fmt.Errorf("%w: state root is not canonical", ErrProfileUnavailable)
	}
	roots := append([]string{p.Plan.Directory}, p.WritableRoots...)
	for _, writable := range roots {
		canonical, pathErr := config.CanonicalizePath(writable)
		if pathErr != nil || canonical != writable {
			return fmt.Errorf("%w: provider writable root is not canonical: %s", ErrProfileUnavailable, writable)
		}
		if pathContains(root, writable) || pathContains(writable, root) {
			return fmt.Errorf("%w: state root overlaps provider-writable tree %s", ErrProfileUnavailable, writable)
		}
	}
	return nil
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// Validate checks the provider-produced profile against the requested task.
// It validates shape and identity only; the caller remains responsible for
// process admission and launch.
func (p PreparedProfile) Validate(request task.TaskRecord) error {
	if err := validatePlanBindings(p.Plan); err != nil {
		return err
	}
	if !filepath.IsAbs(p.Plan.Executable) || p.ObservedVersion == "" {
		return errors.New("profile must resolve an absolute executable and observed version")
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
// metadata. This is used immediately before a runner starts a provider.
func (p PreparedProfile) Matches(request task.TaskRecord, meta task.MetaRecord) error {
	if err := p.Validate(request); err != nil {
		return err
	}
	inputs, err := task.NormalizeInputFiles(p.Plan.InputFiles)
	if err != nil {
		return err
	}
	outputs, err := task.NormalizeOutputArtifacts(p.Plan.OutputArtifacts)
	if err != nil {
		return err
	}
	if p.Plan.Executable != meta.ProviderExecutable || p.ObservedVersion != meta.ProviderVersion || !environmentMatches(p, meta) || !effectiveConfigMatches(p, meta) || !p.Plan.Predicate.Equal(meta.Predicate) || !task.CompareInputFiles(inputs, meta.InputFiles) || !task.CompareOutputArtifacts(outputs, meta.OutputArtifacts) || p.Plan.OutputWriterContract != meta.OutputWriterContract {
		return task.ErrIdentityMismatch
	}
	return nil
}

func environmentMatches(profile PreparedProfile, meta task.MetaRecord) bool {
	if meta.Environment == nil {
		return true
	}
	return slices.Equal(profile.Plan.Environment, meta.Environment)
}

func effectiveConfigMatches(profile PreparedProfile, meta task.MetaRecord) bool {
	if meta.Environment == nil {
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

// RuntimeCapability describes the command-level checks performed when a
// launch profile is prepared. HelpArgs are prepended before --help (for
// example, Codex uses ["exec"]); RequiredFlags are the flags the adapter puts
// in its argv. These are requirements, not a release manifest.
type RuntimeCapability struct {
	HelpArgs      []string `json:"help_args,omitempty"`
	RequiredFlags []string `json:"required_flags"`
}

// Description is the bounded metadata exposed by the providers command.
// Discoverable controls whether this registration appears in that response.
type Description struct {
	ID               string            `json:"id"`
	SupportedModes   []string          `json:"supported_modes"`
	SupportedOptions []string          `json:"supported_options"`
	Continuation     ContinuationMode  `json:"continuation"`
	Runtime          RuntimeCapability `json:"runtime"`
	Discoverable     bool              `json:"-"`
}

// Registration is one explicit catalog entry. A nil Prepare function is
// valid for historical-only predicate registrations and never grants launch
// authority.
type Registration struct {
	Description     Description
	Prepare         PrepareCandidate
	PrepareExisting PrepareExistingCandidate
	Interpreters    []predicate.Interpreter
}
