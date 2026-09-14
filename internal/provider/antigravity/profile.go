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
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/execution"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const ProfileRevision = "agy-1.2.2-darwin-arm64-workspace-write-3"

// Prepared is retained as an adapter-local name for the common prepared
// profile. The common contract owns validation and app-facing compatibility;
// this alias keeps certification tests focused on agy's existing fields.
type Prepared = commonprovider.PreparedProfile

func PrepareCandidate(request task.TaskRecord) (Prepared, error) {
	arguments, err := printArguments(request)
	if err != nil {
		return Prepared{}, err
	}
	identity, err := resolveRuntime()
	if err != nil {
		return Prepared{}, err
	}
	environment, err := prepareEnvironment(os.Environ())
	if err != nil {
		return Prepared{}, err
	}
	inventory, err := InventorySources(InventoryRequest{Home: environment.Home, Workspace: request.CanonicalCwd})
	if err != nil {
		return Prepared{}, err
	}
	effective, err := resolveEffectivePolicy(identity, environment, inventory)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{
		Plan:            execution.Plan{Executable: identity.Executable, Arguments: arguments, Directory: request.CanonicalCwd, Environment: environment.Values, Predicate: NewCurrentPrintInterpreter().Reference()},
		ObservedVersion: identity.Version, Effective: effective, WritableRoots: slices.Clone(environment.WritableRoots),
	}, nil
}

// Prepare builds the agy profile consumed by the app catalog. It wraps the
// existing certified preparation path and only adds the identity observer
// factory; it does not launch a process or change policy/argv semantics.
func Prepare(request task.TaskRecord) (commonprovider.PreparedProfile, error) {
	prepared, err := PrepareCertified(request)
	if err != nil {
		return commonprovider.PreparedProfile{}, err
	}
	prepared.Identity = func(expected task.SessionExpectation, record func(task.SessionIdentity) error) (execution.IdentityObserver, error) {
		return NewIdentityObserver(request.TaskID, expected, record)
	}
	return prepared, nil
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
