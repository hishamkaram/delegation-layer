package task

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/config/policy"
)

// NormalizeRequestedConfig applies pure request defaults before immutable hashing.
// It leaves identity, brief, and execution-directory handling to the storage boundary.
func NormalizeRequestedConfig(r *TaskRecord) error {
	if r == nil {
		return errors.New("nil task record")
	}
	normalized := *r
	if err := normalizeRequestedConfig(&normalized); err != nil {
		return err
	}
	if err := policy.ValidateProvider(normalized.Provider); err != nil {
		return err
	}
	*r = normalized
	return nil
}

func normalizeRequestedConfig(r *TaskRecord) error {
	if r.Mode == "" {
		r.Mode = r.RequestedConfig.Permission
		if r.Mode == "" {
			r.Mode = policy.DefaultMode
		}
	}
	if r.RequestedConfig.Permission == "" {
		r.RequestedConfig.Permission = r.Mode
	}
	if r.RequestedConfig.Budget == "" && r.BudgetNanos > 0 {
		r.RequestedConfig.Budget = time.Duration(r.BudgetNanos).String()
	}
	duration, err := policy.ParseBudgetForProvider(r.Provider, r.RequestedConfig.Budget)
	if err != nil {
		return err
	}
	if r.BudgetNanos != 0 && r.BudgetNanos != int64(duration) {
		return errors.New("requested budget does not match budget_nanos")
	}
	r.BudgetNanos = int64(duration)
	r.RequestedConfig.Budget = duration.String()
	r.RequestedConfig.OutputSilenceTimeout, err = NormalizeOutputSilenceTimeout(r.RequestedConfig.OutputSilenceTimeout)
	if err != nil {
		return err
	}
	r.RequestedConfig.NativeTimeout, err = normalizedNativeTimeout(r.Provider, r.RequestedConfig.NativeTimeout, r.BudgetNanos)
	if err != nil {
		return err
	}
	return validateRequestedConfig(r.Provider, r.RequestedConfig, r.Mode, r.BudgetNanos)
}

func normalizedNativeTimeout(provider, value string, budget int64) (string, error) {
	if value == "" {
		return "", nil
	}
	if err := policy.ValidateProvider(provider); err != nil {
		return "", err
	}
	duration, err := parseNativeTimeout(value)
	if err != nil {
		return "", err
	}
	if err := checkNativeTimeoutBudget(duration, time.Duration(budget)); err != nil {
		return "", err
	}
	return duration.String(), nil
}

func validateNativeTimeout(provider string, request TaskConfig, budget int64) error {
	normalized, err := normalizedNativeTimeout(provider, request.NativeTimeout, budget)
	if err != nil {
		return err
	}
	if normalized != request.NativeTimeout {
		return errors.New("native timeout must be normalized")
	}
	return nil
}

func validateRequestedConfig(provider string, request TaskConfig, mode string, budget int64) error {
	if normalized, err := NormalizeOutputSilenceTimeout(request.OutputSilenceTimeout); err != nil {
		return err
	} else if normalized != request.OutputSilenceTimeout {
		return errors.New("output silence timeout must be normalized")
	}
	if mode == "" || request.Permission == "" || request.Permission != mode {
		return errors.New("requested permission must match the explicit task mode")
	}
	if err := policy.ValidateMode(mode); err != nil {
		return err
	}
	duration, err := policy.ParseBudgetForProvider(provider, request.Budget)
	if err != nil {
		return err
	}
	if request.Budget != duration.String() || int64(duration) != budget {
		return errors.New("requested budget must be normalized and match budget_nanos")
	}
	if err := validateModelEffort(request.Model, request.Effort); err != nil {
		return err
	}
	return ValidateReviewTarget(request.ReviewTarget)
}

// ValidateReviewTarget checks the persisted shape without deciding which
// provider supports it. Provider catalogs apply that capability decision.
func ValidateReviewTarget(target *ReviewTarget) error {
	if target == nil {
		return nil
	}
	switch target.Kind {
	case ReviewTargetUncommitted:
		if target.Value != "" {
			return errors.New("uncommitted review target cannot have a value")
		}
	case ReviewTargetBase, ReviewTargetCommit:
		if !validReviewTargetValue(target.Value) {
			return errors.New("review target value must be a nonblank literal")
		}
	default:
		return fmt.Errorf("unknown review target %q", target.Kind)
	}
	return nil
}

func validReviewTargetValue(value string) bool {
	if !nonblank(value) || len(value) > MaxControlRecordSize || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f || character >= 0x80 && character <= 0x9f {
			return false
		}
	}
	return true
}

func validateTaskConfiguration(r *TaskRecord) error {
	if err := policy.ValidateProvider(r.Provider); err != nil {
		return err
	}
	if err := validateRequestedConfig(r.Provider, r.RequestedConfig, r.Mode, r.BudgetNanos); err != nil {
		return err
	}
	if err := validateNativeTimeout(r.Provider, r.RequestedConfig, r.BudgetNanos); err != nil {
		return err
	}
	// Saved evidence outlives the execution directory. Fresh execution authority
	// additionally validates the current filesystem at the storage boundary.
	if !IsCanonicalCwd(r.CanonicalCwd) {
		return errors.New("saved task cwd must be a clean absolute path")
	}
	return validatePriorSession(r)
}

// IsCanonicalCwd reports whether value has the lexical shape saved as a task
// working directory: nonblank, free of NUL bytes, absolute, and already clean.
// It does not consult the filesystem, resolve links, or require UTF-8.
func IsCanonicalCwd(value string) bool {
	return nonblank(value) && filepath.IsAbs(value) && filepath.Clean(value) == value
}

func validatePriorSession(r *TaskRecord) error {
	if r.PriorSession == nil {
		return nil
	}
	prior := r.PriorSession
	if prior.Provider != r.Provider || !nonblank(prior.ConversationID) || prior.PredecessorTaskID == r.TaskID {
		return errors.New("invalid prior session binding")
	}
	return ValidateTaskID(prior.PredecessorTaskID)
}

func nonblank(value string) bool {
	return strings.TrimSpace(value) != "" && !strings.ContainsRune(value, '\x00')
}

func validateTimestamp(value string) error {
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		return fmt.Errorf("invalid required RFC3339 timestamp: %w", err)
	}
	return nil
}

// ValidatePredicateRef checks record shape, without requiring the implementation
// to remain installed. Recovery performs a separate exact registry lookup.
func ValidatePredicateRef(ref PredicateRef) error {
	if err := policy.ValidateProvider(ref.Adapter); err != nil {
		return err
	}
	if ref.Mode == "" {
		return errors.New("missing predicate mode")
	}
	if err := policy.ValidateMode(ref.Mode); err != nil {
		return err
	}
	if !nonblank(ref.Version) {
		return errors.New("missing predicate version")
	}
	return ValidateSHA256(ref.SHA256)
}

// ValidateSupervisorRef checks persisted supervisor coordinates and provenance.
func ValidateSupervisorRef(ref SupervisorRef) error {
	if err := ref.ExecutableIdentity.Validate(); err != nil {
		return err
	}
	if ref.ExecutableIdentity == ExecutableIdentityInstalledPathV1 {
		if err := validateInstalledSupervisorRef(ref); err != nil {
			return err
		}
	}
	for _, executable := range []struct {
		name  string
		value string
	}{
		{name: "client", value: ref.ClientExecutable},
		{name: "daemon", value: ref.DaemonExecutable},
	} {
		if err := validateOptionalSupervisorExecutable(executable.name, executable.value); err != nil {
			return err
		}
	}
	for _, digest := range []string{ref.ClientSHA256, ref.DaemonSHA256, ref.ResolvedConfigSHA256} {
		if digest != "" {
			if err := ValidateSHA256(digest); err != nil {
				return err
			}
		}
	}
	if err := validateSupervisorResolution(ref); err != nil {
		return err
	}
	if !filepath.IsAbs(ref.ConfigPath) || filepath.Clean(ref.ConfigPath) != ref.ConfigPath {
		return errors.New("supervisor config path must be canonical and absolute")
	}
	if !nonblank(ref.Endpoint) || !nonblank(ref.ObservedVersion) {
		return errors.New("missing supervisor endpoint or version")
	}
	return ValidateSHA256(ref.ConfigDigest)
}

func validateInstalledSupervisorRef(ref SupervisorRef) error {
	if ref.ClientSHA256 != "" || ref.DaemonSHA256 != "" {
		return errors.New("installed supervisor identity contains executable digests")
	}
	if ref.ClientExecutable == "" {
		return errors.New("missing saved supervisor client executable")
	}
	return ValidateSHA256(ref.ResolvedConfigSHA256)
}

func validateSupervisorResolution(ref SupervisorRef) error {
	if ref.ResolutionOS == "" {
		return validateLegacySupervisorResolution(ref)
	}
	if ref.ResolutionOS != "linux" && ref.ResolutionOS != "darwin" {
		return errors.New("supervisor resolution has an unsupported operating system")
	}
	if err := validateRequiredResolutionPaths(ref); err != nil {
		return err
	}
	if err := validateResolutionPathOptional("working directory", ref.ResolutionCwd); err != nil {
		return err
	}
	if err := validateResolutionPathOptional("runtime directory", ref.ResolutionRuntime); err != nil {
		return err
	}
	if ref.ResolutionUsername == "" || strings.ContainsAny(ref.ResolutionUsername, "/\\\x00") {
		return errors.New("supervisor resolution username is invalid")
	}
	return nil
}

func validateLegacySupervisorResolution(ref SupervisorRef) error {
	if ref.ResolutionHome != "" || ref.ResolutionDataLocal != "" || ref.ResolutionConfig != "" || ref.ResolutionRuntime != "" || ref.ResolutionUsername != "" {
		return errors.New("supervisor resolution is incomplete")
	}
	return validateResolutionPathOptional("working directory", ref.ResolutionCwd)
}

func validateRequiredResolutionPaths(ref SupervisorRef) error {
	for _, path := range []struct {
		name  string
		value string
	}{
		{"home", ref.ResolutionHome},
		{"data directory", ref.ResolutionDataLocal},
		{"config directory", ref.ResolutionConfig},
	} {
		if err := validateResolutionPath(path.name, path.value); err != nil {
			return err
		}
	}
	return nil
}

func validateResolutionPathOptional(name, value string) error {
	if value == "" {
		return nil
	}
	return validateResolutionPath(name, value)
}

func validateResolutionPath(name, value string) error {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("supervisor resolution %s must be a clean absolute path", name)
	}
	return nil
}

func validateOptionalSupervisorExecutable(name, value string) error {
	if value == "" {
		return nil
	}
	if !nonblank(value) || !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return fmt.Errorf("supervisor %s must be a clean absolute path", name)
	}
	return nil
}

// ValidateEnvironment checks the bounded nonsecret environment persisted with
// a prepared task. A nil or empty environment preserves compatibility with
// records written before task environments became explicit.
func ValidateEnvironment(values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, entry := range values {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(key, '\x00') || strings.ContainsRune(value, '\x00') {
			return errors.New("environment entries must be nonempty key-value pairs without NUL bytes")
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate environment key %q", key)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// ValidateProviderRuntimeIdentity validates an optional final-launch
// observation. Empty identities preserve compatibility with records written
// before runtime identity evidence was recorded. A nonempty identity names its
// resolved executable; the reported version and executable digest are optional
// observations, validated only when present.
func ValidateProviderRuntimeIdentity(identity ProviderRuntimeIdentity) error {
	if identity == (ProviderRuntimeIdentity{}) {
		return nil
	}
	if !filepath.IsAbs(identity.Executable) || filepath.Clean(identity.Executable) != identity.Executable {
		return errors.New("provider runtime executable must be a resolved absolute path")
	}
	if identity.Version != "" {
		if err := validateProviderRuntimeVersion(identity.Version); err != nil {
			return err
		}
	}
	if identity.SHA256 != "" {
		if err := ValidateSHA256(identity.SHA256); err != nil {
			return fmt.Errorf("provider runtime executable digest: %w", err)
		}
	}
	return nil
}

func validateProviderRuntimeVersion(version string) error {
	if !nonblank(version) {
		return errors.New("provider runtime version must not be blank")
	}
	if !utf8.ValidString(version) {
		return errors.New("provider runtime version must be valid UTF-8")
	}
	for _, character := range version {
		if unicode.IsControl(character) {
			return errors.New("provider runtime version contains control characters")
		}
	}
	return nil
}

// ValidateProviderRuntimeEvidenceIdentity applies the terminal-record bound
// after validating the provider identity itself. Capability observations keep
// accepting any valid nonempty version; only the bounded terminal evidence
// representation has this storage safeguard.
func ValidateProviderRuntimeEvidenceIdentity(identity ProviderRuntimeIdentity) error {
	if err := ValidateProviderRuntimeIdentity(identity); err != nil {
		return err
	}
	if identity.Executable != "" && len(identity.Version) > MaxProviderRuntimeEvidenceVersionBytes {
		return fmt.Errorf("%w: provider runtime version exceeds terminal evidence bound", ErrControlRecordTooBig)
	}
	return nil
}

func validateMetaConfiguration(r *MetaRecord) error {
	if err := ValidateEnvironment(r.Environment); err != nil {
		return err
	}
	if err := ValidatePredicateRef(r.Predicate); err != nil {
		return err
	}
	duration, err := policy.ParseBudgetForProvider(r.Predicate.Adapter, r.RequestedConfig.Budget)
	if err != nil {
		return err
	}
	if err := validateRequestedConfig(r.Predicate.Adapter, r.RequestedConfig, r.Predicate.Mode, int64(duration)); err != nil {
		return err
	}
	if err := validateNativeTimeout(r.Predicate.Adapter, r.RequestedConfig, int64(duration)); err != nil {
		return err
	}
	if !nonblank(r.Containment) || !nonblank(r.Approval) || r.Containment != r.EffectiveConfig.Containment || r.Approval != r.EffectiveConfig.Approval {
		return errors.New("missing or inconsistent effective containment and approval")
	}
	if err := ValidateEffectiveConfig(r.EffectiveConfig); err != nil {
		return err
	}
	return validateMetaRuntime(r)
}

func validateMetaRuntime(r *MetaRecord) error {
	if !filepath.IsAbs(r.ProviderExecutable) || filepath.Clean(r.ProviderExecutable) != r.ProviderExecutable {
		return errors.New("provider executable must be a resolved absolute path")
	}
	if r.RunnerExecutable != "" && (!filepath.IsAbs(r.RunnerExecutable) || filepath.Clean(r.RunnerExecutable) != r.RunnerExecutable) {
		return errors.New("runner executable must be a resolved absolute path")
	}
	if r.RunnerOwnership != "" && r.RunnerOwnership != RunnerOwnershipManaged && r.RunnerOwnership != RunnerOwnershipCustom {
		return errors.New("invalid runner ownership")
	}
	if err := validateMetaRuntimeObservation(r); err != nil {
		return err
	}
	if !nonblank(r.PublisherBuild) || !nonblank(r.PublisherVersion) {
		return errors.New("missing publisher build/version")
	}
	if err := ValidateSupervisorRef(r.SupervisorConfig); err != nil {
		return err
	}
	return validateTimestamp(r.CreatedAt)
}

func validateMetaRuntimeObservation(r *MetaRecord) error {
	if r.RuntimePreflightSHA256 != "" {
		if err := ValidateSHA256(r.RuntimePreflightSHA256); err != nil || r.ProviderVersion != "" || !r.EnvironmentRecorded {
			return errors.New("invalid pending runtime preflight")
		}
		return nil
	}
	if r.ProviderVersion != "" && !nonblank(r.ProviderVersion) {
		return errors.New("blank provider version")
	}
	return nil
}

func validateSessionFields(provider, timestamp string) error {
	if err := policy.ValidateProvider(provider); err != nil {
		return err
	}
	return validateTimestamp(timestamp)
}

// ValidateFreshSupervisorRef requires a complete binding without reading files.
// Unmarked records retain their historical digest requirements.
func ValidateFreshSupervisorRef(ref SupervisorRef) error {
	if err := ValidateSupervisorRef(ref); err != nil {
		return err
	}
	if ref.ExecutableIdentity == ExecutableIdentityInstalledPathV1 {
		return nil
	}
	if ref.ClientExecutable == "" {
		return errors.New("missing saved supervisor client executable")
	}
	if err := ValidateSHA256(ref.ClientSHA256); err != nil {
		return err
	}
	return ValidateSHA256(ref.ResolvedConfigSHA256)
}
