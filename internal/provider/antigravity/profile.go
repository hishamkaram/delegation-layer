package antigravity

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const ProfileRevision = "agy-workspace-write-v1"

const unattendedApproval = "always-proceed"

// PrepareCandidate resolves only static task inputs and describes the
// supervised runtime capability probe. The core owns every provider process;
// the finalizer receives only the resulting nonsecret runtime facts.
func PrepareCandidate(request task.TaskRecord) (commonprovider.ProfileCandidate, error) {
	return prepareCandidate(request, true)
}

// PrepareExistingCandidate retains the preparation contract of admitted tasks.
func PrepareExistingCandidate(request task.TaskRecord, meta task.MetaRecord) (commonprovider.ProfileCandidate, error) {
	native := meta.EffectiveConfig.Policy != nil && meta.EffectiveConfig.Policy.ProfileRevision == commonprovider.NativeProfileRevision
	if !native {
		return prepareCandidate(request, false)
	}
	approval := meta.EffectiveConfig.Approval
	if approval != "accept-edits" && approval != unattendedApproval {
		return commonprovider.ProfileCandidate{}, fmt.Errorf("%w: unknown recorded native approval", ErrUnsupportedProfile)
	}
	var recordedRoots []string
	if meta.EffectiveConfig.Policy != nil {
		recordedRoots = meta.EffectiveConfig.Policy.WritableRoots
	}
	return prepareCandidateWithEnvironment(request, true, approval, meta.Environment, recordedRoots)
}

func prepareCandidate(request task.TaskRecord, native bool) (commonprovider.ProfileCandidate, error) {
	return prepareCandidateWithEnvironment(request, native, unattendedApproval, nil, nil)
}

func prepareCandidateWithEnvironment(request task.TaskRecord, native bool, approval string, recordedEnvironment, recordedRoots []string) (commonprovider.ProfileCandidate, error) {
	arguments, err := candidateArguments(request, native, approval)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	prepareEnvironment := prepareProfileEnvironment
	if recordedEnvironment != nil || len(recordedRoots) > 0 {
		prepareEnvironment = prepareHistoricalNativeEnvironment
	}
	environment, err := prepareEnvironment(candidateEnvironmentValues(recordedEnvironment), native)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	environment.WritableRoots = recordedWritableRoots(environment.WritableRoots, recordedRoots)
	located, err := commonprovider.LocateCLI("agy")
	if err != nil {
		return commonprovider.ProfileCandidate{}, fmt.Errorf("%w: %w", ErrUnsupportedProfile, err)
	}
	requirements := candidateRuntimeRequirements(native, approval)
	definition, err := commonprovider.NewRuntimeInspectionDefinition(located, request.CanonicalCwd, environment.Values, requirements)
	if err != nil {
		return commonprovider.ProfileCandidate{}, fmt.Errorf("%w: runtime inspection: %w", ErrUnsupportedProfile, err)
	}
	inventory, err := candidateInventory(request, environment, native)
	if err != nil {
		return commonprovider.ProfileCandidate{}, err
	}
	return commonprovider.ProfileCandidate{
		Directory:     request.CanonicalCwd,
		WritableRoots: slices.Clone(environment.WritableRoots),
		Inspection:    &definition,
		Finalize: func(data json.RawMessage, _ time.Time) (commonprovider.PreparedProfile, error) {
			facts, decodeErr := commonprovider.DecodeInspectionFacts(data)
			if decodeErr != nil || facts.Runtime == nil || facts.Native != nil {
				return commonprovider.PreparedProfile{}, fmt.Errorf("%w: invalid runtime inspection facts", ErrUnsupportedProfile)
			}
			identity, finalErr := decodeRuntimeIdentity(facts, located)
			if finalErr != nil {
				return commonprovider.PreparedProfile{}, finalErr
			}
			effective, finalErr := candidateEffectiveConfig(request, environment, identity, inventory, native, approval)
			if finalErr != nil {
				return commonprovider.PreparedProfile{}, finalErr
			}
			prepared := commonprovider.PreparedProfile{
				Plan:            execution.Plan{Executable: identity.Executable, Arguments: slices.Clone(arguments), Directory: request.CanonicalCwd, Environment: slices.Clone(environment.Values), Predicate: NewCurrentPrintInterpreter().Reference()},
				ObservedVersion: identity.Version, Effective: effective, WritableRoots: slices.Clone(environment.WritableRoots),
				Identity: func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
					return NewIdentityObserver(request.TaskID, expected, record)
				},
			}
			if finalErr = prepared.Validate(request); finalErr != nil {
				return commonprovider.PreparedProfile{}, finalErr
			}
			return prepared, nil
		},
	}, nil
}

func candidateArguments(request task.TaskRecord, native bool, approval string) ([]string, error) {
	arguments, err := printArguments(request)
	if err != nil {
		return nil, err
	}
	if native {
		arguments = slices.DeleteFunc(arguments, func(argument string) bool { return argument == "--disable-slash-commands" })
		if approval == unattendedApproval {
			arguments = append(arguments, "--dangerously-skip-permissions")
		}
	}
	return arguments, nil
}

func candidateEnvironmentValues(recorded []string) []string {
	if recorded != nil {
		return recorded
	}
	return os.Environ()
}

func recordedWritableRoots(current, recorded []string) []string {
	if len(recorded) != 0 {
		return slices.Clone(recorded)
	}
	return current
}

func candidateRuntimeRequirements(native bool, approval string) commonprovider.RuntimeCapability {
	requirements := RuntimeRequirements()
	if !native || approval != unattendedApproval {
		requirements.RequiredFlags = slices.DeleteFunc(requirements.RequiredFlags, func(flag string) bool { return flag == "--dangerously-skip-permissions" })
	}
	if !native {
		requirements.RequiredFlags = slices.Insert(requirements.RequiredFlags, 5, "--disable-slash-commands")
	}
	return requirements
}

func candidateInventory(request task.TaskRecord, environment profileEnvironment, native bool) (PolicyInventory, error) {
	if native {
		return PolicyInventory{}, nil
	}
	return InventorySources(InventoryRequest{Home: environment.Home, Workspace: request.CanonicalCwd})
}

func resolveEffectivePolicy(identity runtimeIdentity, environment profileEnvironment, inventory PolicyInventory) (task.EffectiveConfig, error) {
	if !inventory.Project.BaselineCandidate {
		return task.EffectiveConfig{}, fmt.Errorf("%w: only the sparse default CLI project is supported", ErrUnsupportedProfile)
	}
	settings, err := validatePolicySources(inventory)
	if err != nil {
		return task.EffectiveConfig{}, err
	}
	if err = validateWorkspaceTrust(settings.TrustedWorkspaces, inventory.Workspace); err != nil {
		return task.EffectiveConfig{}, err
	}
	settings = normalizeSettings(settings)
	details := &task.PolicyDetails{ProfileRevision: ProfileRevision, RuntimeSHA256: identity.SHA256, Workspace: inventory.Workspace, WritableRoots: slices.Clone(environment.WritableRoots)}
	for _, source := range inventory.Sources {
		details.Sources = append(details.Sources, task.PolicySourceDigest{Path: source.Path, Kind: "file:" + string(source.Kind), Present: source.Present, SHA256: source.SHA256})
	}
	for _, directory := range inventory.Directories {
		details.Sources = append(details.Sources, task.PolicySourceDigest{Path: directory.Path, Kind: "directory:" + string(directory.Kind), Present: directory.Present, SHA256: directory.SHA256})
	}
	slices.SortFunc(details.Sources, func(a, b task.PolicySourceDigest) int {
		if order := cmp.Compare(a.Path, b.Path); order != 0 {
			return order
		}
		return cmp.Compare(a.Kind, b.Kind)
	})
	effective := task.EffectiveConfig{Containment: Mode, Approval: "accept-edits:" + string(settings.ToolPermission) + ":headless-deny", Policy: details}
	encoded, err := task.MarshalCanonical(effective)
	if err != nil {
		return task.EffectiveConfig{}, err
	}
	effective.Digest = task.ComputeSHA256(encoded)
	if err = task.ValidateEffectiveConfig(effective); err != nil {
		return task.EffectiveConfig{}, err
	}
	return effective, nil
}

func validatePolicySources(inventory PolicyInventory) (Settings, error) {
	settings, err := ParseSettings([]byte(`{}`))
	if err != nil {
		return Settings{}, err
	}
	for _, source := range inventory.Sources {
		if !source.Present {
			continue
		}
		if source.Kind == KindSettings {
			settings, err = ParseSettings(source.Data)
		} else {
			err = validateOtherPolicySource(source)
		}
		if err != nil {
			return Settings{}, fmt.Errorf("policy source %s: %w", source.Path, err)
		}
	}
	return settings, nil
}

func validateOtherPolicySource(source InventorySource) error {
	switch source.Kind {
	case KindSharedConfig:
		return validateSharedConfig(source.Data)
	case KindProject:
		return validateDefaultProject(source.Data)
	case KindDefaultID:
		if strings.TrimSpace(string(source.Data)) != defaultProjectID {
			return projectPolicyError("unsupported default project identity")
		}
	case KindProjectMap:
		return projectPolicyError("project mappings are not supported by this profile")
	case KindRule, KindMetadata:
		// Rules are model context, not a grant of tool or filesystem privileges.
	case KindHook, KindMCP, KindPlugin, KindAgent, KindSkill, KindWorkflow, KindKeybindings, KindCustomization:
		if len(source.Data) != 0 {
			return projectPolicyError("nonempty extension/control source is unsupported")
		}
	case KindSettings:
		return projectPolicyError("settings must use the typed settings parser")
	default:
		return projectPolicyError("unknown policy source kind")
	}
	return nil
}

func validateSharedConfig(data []byte) error {
	if !utf8.Valid(data) || task.ValidateJSONStructure(data) != nil {
		return projectPolicyError("invalid shared settings JSON")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return projectPolicyError("shared settings must be an object")
	}
	if len(fields) == 0 {
		return nil
	}
	value, ok := fields["userSettings"]
	if !ok || len(fields) != 1 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return projectPolicyError("unsupported shared settings fields")
	}
	return validateSharedUserSettings(value)
}

func validateSharedUserSettings(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return projectPolicyError("shared user settings must be an object")
	}
	for key, value := range fields {
		if key != "remoteControlHostname" || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return projectPolicyError("unsupported shared user setting")
		}
		var hostname string
		if err := json.Unmarshal(value, &hostname); err != nil || strings.ContainsRune(hostname, '\x00') {
			return projectPolicyError("invalid remote-control display hostname")
		}
	}
	return nil
}

func validateWorkspaceTrust(trusted []string, workspace string) error {
	matched := false
	for _, path := range trusted {
		canonical, err := config.CanonicalizePath(path)
		if err != nil || canonical != path {
			return projectPolicyError("trusted workspace path is not canonical")
		}
		relative, err := filepath.Rel(path, workspace)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			matched = true
		}
	}
	if matched {
		return nil
	}
	return projectPolicyError("workspace has no matching configured trust root")
}

func candidateEffectiveConfig(request task.TaskRecord, environment profileEnvironment, identity runtimeIdentity, inventory PolicyInventory, native bool, approval string) (task.EffectiveConfig, error) {
	if native {
		return commonprovider.NativeEffectiveConfig(request, environment.WritableRoots, identity.SHA256, approval)
	}
	return resolveEffectivePolicy(identity, environment, inventory)
}
