package task

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
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
	if err := config.ValidateProvider(normalized.Provider); err != nil {
		return err
	}
	*r = normalized
	return nil
}

func normalizeRequestedConfig(r *TaskRecord) error {
	if r.Mode == "" {
		r.Mode = r.RequestedConfig.Permission
		if r.Mode == "" {
			r.Mode = config.DefaultMode
		}
	}
	if r.RequestedConfig.Permission == "" {
		r.RequestedConfig.Permission = r.Mode
	}
	if r.RequestedConfig.Budget == "" && r.BudgetNanos > 0 {
		r.RequestedConfig.Budget = time.Duration(r.BudgetNanos).String()
	}
	duration, err := config.ParseBudget(r.RequestedConfig.Budget)
	if err != nil {
		return err
	}
	if r.BudgetNanos != 0 && r.BudgetNanos != int64(duration) {
		return errors.New("requested budget does not match budget_nanos")
	}
	r.BudgetNanos = int64(duration)
	r.RequestedConfig.Budget = duration.String()
	r.RequestedConfig.NativeTimeout, err = normalizedNativeTimeout(r.Provider, r.RequestedConfig.NativeTimeout, r.BudgetNanos)
	if err != nil {
		return err
	}
	return validateRequestedConfig(r.RequestedConfig, r.Mode, r.BudgetNanos)
}

func normalizedNativeTimeout(provider, value string, budget int64) (string, error) {
	if value == "" {
		return "", nil
	}
	if provider != config.ProviderAntigravityPrint {
		return "", errors.New("native timeout is supported only for antigravity:print")
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 || int64(duration) > budget {
		return "", errors.New("native timeout must be positive and no greater than the task budget")
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

func validateRequestedConfig(request TaskConfig, mode string, budget int64) error {
	if mode == "" || request.Permission == "" || request.Permission != mode {
		return errors.New("requested permission must match the explicit task mode")
	}
	if err := config.ValidateMode(mode); err != nil {
		return err
	}
	duration, err := config.ParseBudget(request.Budget)
	if err != nil {
		return err
	}
	if request.Budget != duration.String() || int64(duration) != budget {
		return errors.New("requested budget must be normalized and match budget_nanos")
	}
	for _, value := range []string{request.Model, request.Effort} {
		if strings.TrimSpace(value) != value || strings.ContainsRune(value, '\x00') {
			return errors.New("model and effort must be nonblank literal values when supplied")
		}
	}
	return nil
}

func validateTaskConfiguration(r *TaskRecord) error {
	if err := config.ValidateProvider(r.Provider); err != nil {
		return err
	}
	if err := validateRequestedConfig(r.RequestedConfig, r.Mode, r.BudgetNanos); err != nil {
		return err
	}
	if err := validateNativeTimeout(r.Provider, r.RequestedConfig, r.BudgetNanos); err != nil {
		return err
	}
	// Saved evidence outlives the execution directory. Fresh execution authority
	// additionally validates the current filesystem at the storage boundary.
	if !nonblank(r.CanonicalCwd) || !filepath.IsAbs(r.CanonicalCwd) || filepath.Clean(r.CanonicalCwd) != r.CanonicalCwd {
		return errors.New("saved task cwd must be a clean absolute path")
	}
	return validatePriorSession(r)
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
	if err := config.ValidateProvider(ref.Adapter); err != nil {
		return err
	}
	if ref.Mode == "" {
		return errors.New("missing predicate mode")
	}
	if err := config.ValidateMode(ref.Mode); err != nil {
		return err
	}
	if !nonblank(ref.Version) {
		return errors.New("missing predicate version")
	}
	return ValidateSHA256(ref.SHA256)
}

// ValidateSupervisorRef checks the complete pinned supervisor coordinates.
func ValidateSupervisorRef(ref SupervisorRef) error {
	if ref.ClientExecutable != "" && (!nonblank(ref.ClientExecutable) || !filepath.IsAbs(ref.ClientExecutable) || filepath.Clean(ref.ClientExecutable) != ref.ClientExecutable) {
		return errors.New("supervisor client must be a clean absolute path")
	}
	for _, digest := range []string{ref.ClientSHA256, ref.ResolvedConfigSHA256} {
		if digest != "" {
			if err := ValidateSHA256(digest); err != nil {
				return err
			}
		}
	}
	if !filepath.IsAbs(ref.ConfigPath) || filepath.Clean(ref.ConfigPath) != ref.ConfigPath {
		return errors.New("supervisor config path must be canonical and absolute")
	}
	if !nonblank(ref.Endpoint) || !nonblank(ref.ObservedVersion) {
		return errors.New("missing supervisor endpoint or version")
	}
	return ValidateSHA256(ref.ConfigDigest)
}

func validateMetaConfiguration(r *MetaRecord) error {
	if err := ValidatePredicateRef(r.Predicate); err != nil {
		return err
	}
	duration, err := config.ParseBudget(r.RequestedConfig.Budget)
	if err != nil {
		return err
	}
	if err := validateRequestedConfig(r.RequestedConfig, r.Predicate.Mode, int64(duration)); err != nil {
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
	if !nonblank(r.ProviderVersion) || !nonblank(r.PublisherBuild) || !nonblank(r.PublisherVersion) {
		return errors.New("missing provider version or publisher build/version")
	}
	if err := ValidateSupervisorRef(r.SupervisorConfig); err != nil {
		return err
	}
	return validateTimestamp(r.CreatedAt)
}

func validateSessionFields(provider, timestamp string) error {
	if err := config.ValidateProvider(provider); err != nil {
		return err
	}
	return validateTimestamp(timestamp)
}

// ValidateFreshSupervisorRef requires the complete Phase2 binding without reading
// external files. The pueue boundary verifies actual executable/configuration bytes.
func ValidateFreshSupervisorRef(ref SupervisorRef) error {
	if err := ValidateSupervisorRef(ref); err != nil {
		return err
	}
	if ref.ClientExecutable == "" {
		return errors.New("missing saved supervisor client executable")
	}
	if err := ValidateSHA256(ref.ClientSHA256); err != nil {
		return err
	}
	return ValidateSHA256(ref.ResolvedConfigSHA256)
}
