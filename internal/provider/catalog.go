package provider

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Catalog is an immutable snapshot of the explicit registrations supplied by
// a composition root. The maps and slices are private so callers cannot alter
// registration or interpreter selection after construction.
type Catalog struct {
	entries     []Registration
	byID        map[string]int
	registry    predicate.Registry
	initialized bool
}

// NewCatalog validates and snapshots one production registration list. IDs and
// exact predicate references are unique across the complete list, including
// historical-only entries.
func NewCatalog(registrations ...Registration) (Catalog, error) {
	entries := make([]Registration, 0, len(registrations))
	byID := make(map[string]int, len(registrations))
	allInterpreters := make([]predicate.Interpreter, 0)
	seenRefs := make(map[string]struct{})

	for _, supplied := range registrations {
		registration, err := normalizeRegistration(supplied)
		if err != nil {
			return Catalog{}, err
		}
		if _, exists := byID[registration.Description.ID]; exists {
			return Catalog{}, fmt.Errorf("%w: %s", ErrDuplicateProvider, registration.Description.ID)
		}
		interpreters, err := validateInterpreters(registration, seenRefs)
		if err != nil {
			return Catalog{}, err
		}
		allInterpreters = append(allInterpreters, interpreters...)
		byID[registration.Description.ID] = len(entries)
		entries = append(entries, registration)
	}

	slices.SortFunc(entries, func(left, right Registration) int {
		return strings.Compare(left.Description.ID, right.Description.ID)
	})
	byID = make(map[string]int, len(entries))
	for index := range entries {
		byID[entries[index].Description.ID] = index
	}
	registry, err := predicate.New(allInterpreters...)
	if err != nil {
		return Catalog{}, fmt.Errorf("building provider predicate registry: %w", err)
	}
	return Catalog{entries: entries, byID: byID, registry: registry, initialized: true}, nil
}

func normalizeRegistration(supplied Registration) (Registration, error) {
	registration := supplied
	if err := validateDescription(registration.Description); err != nil {
		return Registration{}, err
	}
	if registration.Description.Discoverable && registration.Prepare == nil {
		return Registration{}, fmt.Errorf("%w: discoverable provider %s has no preparation", ErrInvalidDescriptor, registration.Description.ID)
	}
	if registration.Prepare != nil && !registration.Description.Discoverable {
		return Registration{}, fmt.Errorf("%w: launchable provider %s must be discoverable", ErrInvalidDescriptor, registration.Description.ID)
	}
	if registration.Prepare != nil && len(registration.Interpreters) == 0 {
		return Registration{}, fmt.Errorf("%w: provider %s has no interpreter", ErrInvalidDescriptor, registration.Description.ID)
	}
	registration.Description.SupportedOptions = slices.Clone(registration.Description.SupportedOptions)
	slices.Sort(registration.Description.SupportedOptions)
	registration.Description.Profiles = slices.Clone(registration.Description.Profiles)
	slices.SortFunc(registration.Description.Profiles, func(left, right CertifiedProfile) int {
		return strings.Compare(profileKey(left), profileKey(right))
	})
	registration.Interpreters = slices.Clone(registration.Interpreters)
	return registration, nil
}

func validateDescription(description Description) error {
	if err := config.ValidateProvider(description.ID); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidDescriptor, err)
	}
	if err := validateOptions(description.SupportedOptions); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidDescriptor, err)
	}
	if err := validateProfiles(description.ID, description.Profiles); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidDescriptor, err)
	}
	if description.Discoverable && len(description.Profiles) == 0 {
		return fmt.Errorf("%w: discoverable provider %s has no certified profile", ErrInvalidDescriptor, description.ID)
	}
	return nil
}

func validateInterpreterCoverage(description Description, interpreterRefs map[string]struct{}) error {
	for _, profile := range description.Profiles {
		if _, ok := interpreterRefs[predicateKey(profile.Predicate)]; !ok {
			return fmt.Errorf("%w: provider %s has no interpreter for profile predicate", ErrInvalidDescriptor, description.ID)
		}
	}
	return nil
}

func validateInterpreters(registration Registration, seenRefs map[string]struct{}) ([]predicate.Interpreter, error) {
	interpreterRefs := make(map[string]struct{}, len(registration.Interpreters))
	for _, interpreter := range registration.Interpreters {
		if isNilInterpreter(interpreter) {
			return nil, fmt.Errorf("%w: provider %s has a nil interpreter", ErrInvalidDescriptor, registration.Description.ID)
		}
		ref := interpreter.Reference()
		if err := task.ValidatePredicateRef(ref); err != nil {
			return nil, fmt.Errorf("%w: provider %s predicate: %w", ErrInvalidDescriptor, registration.Description.ID, err)
		}
		if ref.Adapter != registration.Description.ID {
			return nil, fmt.Errorf("%w: predicate adapter does not match %s", ErrInvalidDescriptor, registration.Description.ID)
		}
		key := predicateKey(ref)
		if _, exists := seenRefs[key]; exists {
			return nil, fmt.Errorf("%w: %s/%s/%s", ErrDuplicatePredicate, ref.Adapter, ref.Mode, ref.Version)
		}
		seenRefs[key] = struct{}{}
		interpreterRefs[key] = struct{}{}
	}
	if err := validateInterpreterCoverage(registration.Description, interpreterRefs); err != nil {
		return nil, err
	}
	return registration.Interpreters, nil
}

func validateOptions(options []string) error {
	seenOptions := make(map[string]struct{}, len(options))
	for _, option := range options {
		if !validOption(option) {
			return fmt.Errorf("unsupported request option %q", option)
		}
		if _, exists := seenOptions[option]; exists {
			return fmt.Errorf("duplicate request option %q", option)
		}
		seenOptions[option] = struct{}{}
	}
	return nil
}

func validateProfiles(providerID string, profiles []CertifiedProfile) error {
	seenProfiles := make(map[string]struct{}, len(profiles))
	for _, profile := range profiles {
		if err := validateProfile(providerID, profile); err != nil {
			return err
		}
		if key := profileKey(profile); !addUnique(seenProfiles, key) {
			return fmt.Errorf("duplicate certified profile for %s", providerID)
		}
	}
	return nil
}

func validateProfile(providerID string, profile CertifiedProfile) error {
	if profile.Mode == "" || profile.Approval == "" || profile.Status == "" || profile.ProviderVersion == "" || profile.OS == "" || profile.Arch == "" || profile.ProfileRevision == "" {
		return fmt.Errorf("incomplete certified profile for %s", providerID)
	}
	if profile.Status != "Executed" {
		return fmt.Errorf("certified profile for %s is not Executed", providerID)
	}
	if err := task.ValidateSHA256(profile.RuntimeSHA256); err != nil {
		return fmt.Errorf("profile runtime SHA-256: %w", err)
	}
	if err := config.ValidateMode(profile.Mode); err != nil {
		return fmt.Errorf("profile mode: %w", err)
	}
	if profile.Predicate.Adapter != providerID {
		return fmt.Errorf("profile predicate adapter does not match %s", providerID)
	}
	if err := task.ValidatePredicateRef(profile.Predicate); err != nil {
		return fmt.Errorf("profile predicate: %w", err)
	}
	if profile.Predicate.Mode != profile.Mode {
		return fmt.Errorf("profile mode does not match its predicate for %s", providerID)
	}
	return nil
}

func validOption(option string) bool {
	switch option {
	case OptionContinuation, OptionEffort, OptionModel, OptionNativeTimeout:
		return true
	default:
		return false
	}
}

func addUnique(values map[string]struct{}, key string) bool {
	if _, exists := values[key]; exists {
		return false
	}
	values[key] = struct{}{}
	return true
}

func profileKey(profile CertifiedProfile) string {
	return strings.Join([]string{profile.Mode, profile.Approval, profile.Status, profile.ProviderVersion, profile.OS, profile.Arch, profile.ProfileRevision, predicateKey(profile.Predicate)}, "\x00")
}

func predicateKey(ref task.PredicateRef) string {
	return strings.Join([]string{ref.Adapter, ref.Mode, ref.Version, ref.SHA256}, "\x00")
}

func isNilInterpreter(interpreter predicate.Interpreter) bool {
	value := reflect.ValueOf(interpreter)
	if !value.IsValid() {
		return true
	}
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	case reflect.Invalid, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr, reflect.Float32, reflect.Float64,
		reflect.Complex64, reflect.Complex128, reflect.Array, reflect.String, reflect.Struct, reflect.UnsafePointer:
		return false
	}
	panic("unreachable reflect.Kind")
}

// IsZero reports whether c has not been constructed by NewCatalog.
func (c Catalog) IsZero() bool { return !c.initialized }

// Lookup returns a defensive copy of one registration by its structural ID.
func (c Catalog) Lookup(id string) (Registration, error) {
	if err := config.ValidateProvider(id); err != nil {
		return Registration{}, err
	}
	index, ok := c.byID[id]
	if !ok {
		return Registration{}, fmt.Errorf("%w: %w: %s", ErrProfileUnavailable, config.ErrUnsupportedProvider, id)
	}
	return cloneRegistration(c.entries[index]), nil
}

func cloneRegistration(registration Registration) Registration {
	registration.Description.SupportedOptions = slices.Clone(registration.Description.SupportedOptions)
	registration.Description.Profiles = slices.Clone(registration.Description.Profiles)
	registration.Interpreters = slices.Clone(registration.Interpreters)
	return registration
}

// Descriptions returns discoverable metadata in deterministic provider-ID
// order. No executable, policy, authentication, supervisor, or filesystem
// probe occurs while producing this snapshot.
func (c Catalog) Descriptions() []Description {
	if c.IsZero() {
		return nil
	}
	result := make([]Description, 0, len(c.entries))
	for _, registration := range c.entries {
		if registration.Description.Discoverable {
			result = append(result, cloneRegistration(registration).Description)
		}
	}
	return result
}

// Registry returns the immutable predicate registry, including historical
// registrations that are intentionally omitted from discovery.
func (c Catalog) Registry() predicate.Registry { return c.registry }

// ValidateRequest checks provider capabilities before any preparation or
// supervisor admission. Structural task validation remains in config/task.
func (c Catalog) ValidateRequest(request task.TaskRecord) error {
	if err := config.ValidateProvider(request.Provider); err != nil {
		return err
	}
	registration, err := c.Lookup(request.Provider)
	if err != nil {
		return err
	}
	if registration.Prepare == nil {
		return fmt.Errorf("%w: %s has no launch profile", ErrProfileUnavailable, request.Provider)
	}
	if request.Mode == "" || request.RequestedConfig.Permission == "" || request.Mode != request.RequestedConfig.Permission {
		return task.ErrIdentityMismatch
	}
	if err = config.ValidateMode(request.Mode); err != nil {
		return err
	}
	if !hasCertifiedProfileMode(registration.Description, request.Mode) {
		return fmt.Errorf("%w: %s does not certify permission profile %s", ErrProfileUnavailable, request.Provider, request.Mode)
	}
	options := []struct {
		name    string
		present bool
	}{
		{name: OptionContinuation, present: request.PriorSession != nil},
		// The explicit default is a provider-default selection, so it does not
		// request an effort override and must retain compatibility with agy's
		// existing persisted request representation.
		{name: OptionEffort, present: request.RequestedConfig.Effort != "" && request.RequestedConfig.Effort != "default"},
		{name: OptionModel, present: request.RequestedConfig.Model != ""},
		{name: OptionNativeTimeout, present: request.RequestedConfig.NativeTimeout != ""},
	}
	for _, option := range options {
		if option.present && !slices.Contains(registration.Description.SupportedOptions, option.name) {
			return fmt.Errorf("%w: %s does not support %s", ErrUnsupportedOption, request.Provider, option.name)
		}
	}
	return nil
}

// Prepare validates capabilities and invokes the adapter's preparation hook.
// The hook is the only provider-specific operation here and must return before
// the caller acquires any submission or start permit.
func (c Catalog) Prepare(request task.TaskRecord) (PreparedProfile, error) {
	if err := c.ValidateRequest(request); err != nil {
		return PreparedProfile{}, err
	}
	registration, err := c.Lookup(request.Provider)
	if err != nil {
		return PreparedProfile{}, err
	}
	profile, err := registration.Prepare(request)
	if err != nil {
		return PreparedProfile{}, fmt.Errorf("%w: %w", ErrProfileUnavailable, err)
	}
	if !hasCertifiedProfilePredicate(registration.Description, request.Mode, profile.Plan.Predicate) {
		return PreparedProfile{}, fmt.Errorf("%w: %s returned an uncertified predicate", ErrProfileUnavailable, request.Provider)
	}
	return profile, nil
}

func hasCertifiedProfileMode(description Description, mode string) bool {
	for _, profile := range description.Profiles {
		if profile.Mode == mode {
			return true
		}
	}
	return false
}

func hasCertifiedProfilePredicate(description Description, mode string, ref task.PredicateRef) bool {
	for _, profile := range description.Profiles {
		if profile.Mode == mode && profile.Predicate.Equal(ref) {
			return true
		}
	}
	return false
}
