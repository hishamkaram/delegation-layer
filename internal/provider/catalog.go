package provider

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

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
	if registration.Description.Continuation == "" {
		// Empty is the historical zero value for registrations created before
		// continuation metadata was added. Preserve the legacy capability when
		// the registration already advertised continuation support.
		if slices.Contains(registration.Description.SupportedOptions, OptionContinuation) {
			registration.Description.Continuation = ContinuationNative
		} else {
			registration.Description.Continuation = ContinuationUnsupported
		}
	}
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
	registration.Description.SupportedModes = slices.Clone(registration.Description.SupportedModes)
	slices.Sort(registration.Description.SupportedModes)
	registration.Description.Runtime.HelpArgs = slices.Clone(registration.Description.Runtime.HelpArgs)
	registration.Description.Runtime.RequiredFlags = slices.Clone(registration.Description.Runtime.RequiredFlags)
	slices.Sort(registration.Description.Runtime.RequiredFlags)
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
	if err := validateModes(description.SupportedModes); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidDescriptor, err)
	}
	if !validContinuationMode(description.Continuation) {
		return fmt.Errorf("%w: provider %s has invalid continuation mode %q", ErrInvalidDescriptor, description.ID, description.Continuation)
	}
	if description.Continuation == ContinuationCheckpoint {
		return fmt.Errorf("%w: provider %s advertises unsupported checkpoint continuation", ErrInvalidDescriptor, description.ID)
	}
	continuationOption := slices.Contains(description.SupportedOptions, OptionContinuation)
	if continuationOption != (description.Continuation == ContinuationNative) {
		return fmt.Errorf("%w: provider %s has inconsistent continuation metadata", ErrInvalidDescriptor, description.ID)
	}
	if err := validateRuntimeCapability(description.Runtime); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidDescriptor, err)
	}
	if description.Discoverable && len(description.SupportedModes) == 0 {
		return fmt.Errorf("%w: discoverable provider %s has no supported mode", ErrInvalidDescriptor, description.ID)
	}
	if description.Discoverable && len(description.Runtime.RequiredFlags) == 0 {
		return fmt.Errorf("%w: discoverable provider %s has no runtime flag requirements", ErrInvalidDescriptor, description.ID)
	}
	return nil
}

func validContinuationMode(mode ContinuationMode) bool {
	switch mode {
	case ContinuationNative, ContinuationCheckpoint, ContinuationUnsupported:
		return true
	default:
		return false
	}
}

func validateInterpreters(registration Registration, seenRefs map[string]struct{}) ([]predicate.Interpreter, error) {
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

func validateModes(modes []string) error {
	seen := make(map[string]struct{}, len(modes))
	for _, mode := range modes {
		if err := config.ValidateMode(mode); err != nil {
			return err
		}
		if _, exists := seen[mode]; exists {
			return fmt.Errorf("duplicate supported mode %q", mode)
		}
		seen[mode] = struct{}{}
	}
	return nil
}

func validateRuntimeCapability(capability RuntimeCapability) error {
	seenHelpArgs := make(map[string]struct{}, len(capability.HelpArgs))
	for _, arg := range capability.HelpArgs {
		if arg == "" || strings.ContainsAny(arg, "\x00 \t\r\n") {
			return fmt.Errorf("invalid runtime help argument %q", arg)
		}
		if _, exists := seenHelpArgs[arg]; exists {
			return fmt.Errorf("duplicate runtime help argument %q", arg)
		}
		seenHelpArgs[arg] = struct{}{}
	}
	seenFlags := make(map[string]struct{}, len(capability.RequiredFlags))
	for _, flag := range capability.RequiredFlags {
		if flag == "" || !strings.HasPrefix(flag, "-") || strings.ContainsAny(flag, "\x00 \t\r\n") {
			return fmt.Errorf("invalid required runtime flag %q", flag)
		}
		if _, exists := seenFlags[flag]; exists {
			return fmt.Errorf("duplicate required runtime flag %q", flag)
		}
		seenFlags[flag] = struct{}{}
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
	registration.Description.SupportedModes = slices.Clone(registration.Description.SupportedModes)
	registration.Description.Runtime.HelpArgs = slices.Clone(registration.Description.Runtime.HelpArgs)
	registration.Description.Runtime.RequiredFlags = slices.Clone(registration.Description.Runtime.RequiredFlags)
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
	if !slices.Contains(registration.Description.SupportedModes, request.Mode) {
		return fmt.Errorf("%w: %s does not support permission mode %s", ErrProfileUnavailable, request.Provider, request.Mode)
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

// Candidate validates capabilities and obtains static preparation inputs. Its
// finalizer retains artifact and effective-profile checks after inspection.
func (c Catalog) Candidate(request task.TaskRecord) (ProfileCandidate, error) {
	return c.candidate(request, nil)
}

// ExistingCandidate reconstructs a profile for an already admitted task. A
// provider may use this boundary to retain a historical preparation contract;
// new admission remains on Candidate.
func (c Catalog) ExistingCandidate(request task.TaskRecord, meta task.MetaRecord) (ProfileCandidate, error) {
	return c.candidate(request, &meta)
}

func (c Catalog) candidate(request task.TaskRecord, existing *task.MetaRecord) (ProfileCandidate, error) {
	if err := c.ValidateRequest(request); err != nil {
		return ProfileCandidate{}, err
	}
	registration, err := c.Lookup(request.Provider)
	if err != nil {
		return ProfileCandidate{}, err
	}
	var candidate ProfileCandidate
	if existing != nil && registration.PrepareExisting != nil {
		candidate, err = registration.PrepareExisting(request, *existing)
	} else {
		candidate, err = registration.Prepare(request)
	}
	if err != nil {
		return ProfileCandidate{}, fmt.Errorf("%w: %w", ErrProfileUnavailable, err)
	}
	if candidate.Directory != request.CanonicalCwd {
		return ProfileCandidate{}, task.ErrIdentityMismatch
	}
	if candidate.Finalize == nil {
		return ProfileCandidate{}, fmt.Errorf("%w: missing candidate finalizer", ErrProfileUnavailable)
	}
	candidate.WritableRoots = slices.Clone(candidate.WritableRoots)
	if candidate.Inspection != nil {
		definition, _, snapshotErr := candidate.Inspection.Snapshot()
		if snapshotErr != nil {
			return ProfileCandidate{}, snapshotErr
		}
		candidate.Inspection = &definition
	}
	directory := candidate.Directory
	writableRoots := slices.Clone(candidate.WritableRoots)
	finalize := candidate.Finalize
	registry := c.registry
	candidate.Finalize = func(facts json.RawMessage, now time.Time) (PreparedProfile, error) {
		profile, finalizeErr := finalize(facts, now)
		if finalizeErr != nil {
			return PreparedProfile{}, fmt.Errorf("%w: %w", ErrProfileUnavailable, finalizeErr)
		}
		if profile.Plan.Directory != directory || !slices.Equal(profile.WritableRoots, writableRoots) {
			return PreparedProfile{}, task.ErrIdentityMismatch
		}
		if _, resolveErr := registry.Resolve(profile.Plan.Predicate); resolveErr != nil {
			return PreparedProfile{}, fmt.Errorf("%w: provider returned an unregistered predicate: %w", ErrProfileUnavailable, resolveErr)
		}
		return finalizeCatalogProfile(request, profile)
	}
	return candidate, nil
}

// Prepare preserves the finite preparation path for providers without native
// inspection. It cannot bypass an inspection-dependent candidate's proof.
func (c Catalog) Prepare(request task.TaskRecord) (PreparedProfile, error) {
	candidate, err := c.Candidate(request)
	if err != nil {
		return PreparedProfile{}, err
	}
	if candidate.Inspection != nil {
		return PreparedProfile{}, fmt.Errorf("%w: native inspection is required", ErrProfileUnavailable)
	}
	return candidate.Finalize(nil, time.Now())
}

func finalizeCatalogProfile(request task.TaskRecord, profile PreparedProfile) (PreparedProfile, error) {
	var err error
	profile.Plan.InputFiles, err = task.NormalizeInputFiles(profile.Plan.InputFiles)
	if err != nil {
		return PreparedProfile{}, fmt.Errorf("%w: invalid prepared input files: %w", ErrProfileUnavailable, err)
	}
	profile.Plan.OutputArtifacts, err = task.NormalizeOutputArtifacts(profile.Plan.OutputArtifacts)
	if err != nil {
		return PreparedProfile{}, fmt.Errorf("%w: invalid prepared output artifacts: %w", ErrProfileUnavailable, err)
	}
	if err = task.ValidateInputOutputBindings(profile.Plan.InputFiles, profile.Plan.OutputArtifacts); err != nil {
		return PreparedProfile{}, fmt.Errorf("%w: invalid prepared artifact bindings: %w", ErrProfileUnavailable, err)
	}
	if err = task.ValidateOutputArtifacts(profile.Plan.OutputArtifacts, profile.Plan.OutputWriterContract); err != nil {
		return PreparedProfile{}, fmt.Errorf("%w: invalid prepared output contract: %w", ErrProfileUnavailable, err)
	}
	if err = profile.Validate(request); err != nil {
		return PreparedProfile{}, fmt.Errorf("%w: invalid finalized profile: %w", ErrProfileUnavailable, err)
	}
	return cloneCandidateProfile(profile), nil
}
