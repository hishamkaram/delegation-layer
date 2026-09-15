package antigravity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ErrInvalidSettings identifies a settings source that could not be parsed as
// the bounded settings shape used by the workspace-write profile.
var ErrInvalidSettings = errors.New("invalid antigravity settings")

// ToolPermission is the global terminal/tool approval mode reported by agy.
type ToolPermission string

const (
	ToolPermissionRequestReview  ToolPermission = "request-review"
	ToolPermissionProceedSandbox ToolPermission = "proceed-in-sandbox"
	ToolPermissionStrict         ToolPermission = "strict"
	ToolPermissionAlwaysProceed  ToolPermission = "always-proceed"
)

// ArtifactReviewPolicy is the global artifact approval mode reported by agy.
type ArtifactReviewPolicy string

const (
	ArtifactReviewAsksForReview ArtifactReviewPolicy = "asks-for-review"
	ArtifactReviewAgentDecides  ArtifactReviewPolicy = "agent-decides"
	ArtifactReviewAlwaysProceed ArtifactReviewPolicy = "always-proceed"
)

// PermissionSettings is the documented permission-list shape. The initial
// profile accepts only empty lists; callers can add a versioned rule parser
// when a later profile proves the matching semantics it needs.
type PermissionSettings struct {
	Allow []string `json:"allow,omitempty"`
	Ask   []string `json:"ask,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// Settings is the typed subset of agy's global settings used by the fixed
// workspace-write profile. The scalar defaults are the documented CLI
// defaults. Trusted paths are syntax-checked here and resolved by the caller.
type Settings struct {
	Model                   string               `json:"model,omitempty"`
	TrustedWorkspaces       []string             `json:"trustedWorkspaces,omitempty"`
	ToolPermission          ToolPermission       `json:"toolPermission,omitempty"`
	ArtifactReviewPolicy    ArtifactReviewPolicy `json:"artifactReviewPolicy,omitempty"`
	EnableTerminalSandbox   bool                 `json:"enableTerminalSandbox,omitempty"`
	AllowNonWorkspaceAccess bool                 `json:"allowNonWorkspaceAccess,omitempty"`
	Permissions             PermissionSettings   `json:"permissions,omitempty"`
	present                 settingsPresence
}

type settingsPresence struct {
	model, trustedWorkspaces, toolPermission, artifactReviewPolicy bool
	enableTerminalSandbox, allowNonWorkspaceAccess, permissions    bool
}

// HasModel reports whether the source explicitly supplied model, even when it
// supplied an empty string. The other presence methods preserve sparse source
// semantics for resolver digest construction.
func (s Settings) HasModel() bool                { return s.present.model }
func (s Settings) HasTrustedWorkspaces() bool    { return s.present.trustedWorkspaces }
func (s Settings) HasToolPermission() bool       { return s.present.toolPermission }
func (s Settings) HasArtifactReviewPolicy() bool { return s.present.artifactReviewPolicy }
func (s Settings) HasTerminalSandbox() bool      { return s.present.enableTerminalSandbox }
func (s Settings) HasNonWorkspaceAccess() bool   { return s.present.allowNonWorkspaceAccess }
func (s Settings) HasPermissions() bool          { return s.present.permissions }

// ParseSettings parses one complete agy settings.json value. It performs no
// file, symlink, provider, or environment inspection.
func ParseSettings(data []byte) (Settings, error) {
	settings := Settings{
		ToolPermission:       ToolPermissionRequestReview,
		ArtifactReviewPolicy: ArtifactReviewAsksForReview,
	}
	if !utf8.Valid(data) {
		return Settings{}, settingsError("invalid UTF-8")
	}
	if err := task.ValidateJSONStructure(data); err != nil {
		return Settings{}, settingsError("invalid JSON structure")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return Settings{}, settingsError("settings root is not an object")
	}
	for key, value := range fields {
		if err := applySettingsField(&settings, key, value); err != nil {
			return Settings{}, err
		}
	}
	settings = normalizeSettings(settings)
	if err := ValidateSettings(settings); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

// parseSettings keeps the package-local spelling convenient for provider
// resolver code while exposing ParseSettings to callers in other packages.
func parseSettings(data []byte) (Settings, error) { return ParseSettings(data) }

func applySettingsField(settings *Settings, key string, value json.RawMessage) error {
	switch key {
	case "model":
		return applyModel(settings, value)
	case "trustedWorkspaces":
		return applyTrustedWorkspaces(settings, value)
	case "toolPermission":
		return applyToolPermission(settings, value)
	case "artifactReviewPolicy":
		return applyArtifactReviewPolicy(settings, value)
	case "enableTerminalSandbox":
		return applyTerminalSandbox(settings, value)
	case "allowNonWorkspaceAccess":
		return applyNonWorkspaceAccess(settings, value)
	case "permissions":
		return applyPermissions(settings, value)
	default:
		return settingsError("unknown settings key")
	}
}

func applyModel(settings *Settings, value json.RawMessage) error {
	decoded, err := decodeSettingsString(value, "model")
	if err != nil {
		return err
	}
	settings.Model = decoded
	settings.present.model = true
	return nil
}

func applyTrustedWorkspaces(settings *Settings, value json.RawMessage) error {
	decoded, err := decodeTrustedWorkspaces(value)
	if err != nil {
		return err
	}
	settings.TrustedWorkspaces = decoded
	settings.present.trustedWorkspaces = true
	return nil
}

func applyToolPermission(settings *Settings, value json.RawMessage) error {
	decoded, err := decodeSettingsString(value, "toolPermission")
	if err != nil {
		return err
	}
	settings.ToolPermission = ToolPermission(decoded)
	settings.present.toolPermission = true
	return nil
}

func applyArtifactReviewPolicy(settings *Settings, value json.RawMessage) error {
	decoded, err := decodeSettingsString(value, "artifactReviewPolicy")
	if err != nil {
		return err
	}
	settings.ArtifactReviewPolicy = ArtifactReviewPolicy(decoded)
	settings.present.artifactReviewPolicy = true
	return nil
}

func applyTerminalSandbox(settings *Settings, value json.RawMessage) error {
	decoded, err := decodeSettingsBool(value, "enableTerminalSandbox")
	if err != nil {
		return err
	}
	settings.EnableTerminalSandbox = decoded
	settings.present.enableTerminalSandbox = true
	return nil
}

func applyNonWorkspaceAccess(settings *Settings, value json.RawMessage) error {
	decoded, err := decodeSettingsBool(value, "allowNonWorkspaceAccess")
	if err != nil {
		return err
	}
	settings.AllowNonWorkspaceAccess = decoded
	settings.present.allowNonWorkspaceAccess = true
	return nil
}

func applyPermissions(settings *Settings, value json.RawMessage) error {
	decoded, err := decodePermissions(value)
	if err != nil {
		return err
	}
	settings.Permissions = decoded
	settings.present.permissions = true
	return nil
}

// ValidateSettings checks an already-decoded bounded settings value. It is
// intentionally stricter than the complete agy settings schema: unsupported
// approval bypasses and custom permission rules are outside this profile.
func ValidateSettings(settings Settings) error {
	settings = normalizeSettings(settings)
	toolPermission := settings.ToolPermission
	switch toolPermission {
	case ToolPermissionRequestReview, ToolPermissionProceedSandbox:
	case ToolPermissionStrict, ToolPermissionAlwaysProceed:
		return settingsError("unsupported tool permission")
	default:
		return settingsError("unknown tool permission")
	}
	artifactReviewPolicy := settings.ArtifactReviewPolicy
	switch artifactReviewPolicy {
	case ArtifactReviewAsksForReview:
	case ArtifactReviewAgentDecides, ArtifactReviewAlwaysProceed:
		return settingsError("unsupported artifact review policy")
	default:
		return settingsError("unknown artifact review policy")
	}
	if settings.AllowNonWorkspaceAccess {
		return settingsError("non-workspace access is enabled")
	}
	for _, path := range settings.TrustedWorkspaces {
		if !canonicalSettingsPath(path) {
			return settingsError("invalid trusted workspace path")
		}
	}
	if strings.ContainsRune(settings.Model, '\x00') || !utf8.ValidString(settings.Model) {
		return settingsError("invalid model value")
	}
	if len(settings.Permissions.Allow) != 0 || len(settings.Permissions.Ask) != 0 || len(settings.Permissions.Deny) != 0 {
		return settingsError("nonempty permission rules are unsupported")
	}
	return nil
}

func normalizeSettings(settings Settings) Settings {
	if settings.ToolPermission == "" {
		settings.ToolPermission = ToolPermissionRequestReview
	}
	if settings.ArtifactReviewPolicy == "" {
		settings.ArtifactReviewPolicy = ArtifactReviewAsksForReview
	}
	return settings
}

func decodeTrustedWorkspaces(raw json.RawMessage) ([]string, error) {
	values, err := decodeSettingsStringList(raw, "trustedWorkspaces")
	if err != nil {
		return nil, err
	}
	for _, value := range values {
		if !canonicalSettingsPath(value) {
			return nil, settingsError("invalid trusted workspace path")
		}
	}
	return values, nil
}

func decodePermissions(raw json.RawMessage) (PermissionSettings, error) {
	if isJSONNull(raw) {
		return PermissionSettings{}, settingsError("null permissions value")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return PermissionSettings{}, settingsError("invalid permissions object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil || fields == nil {
		return PermissionSettings{}, settingsError("invalid permissions object")
	}
	var permissions PermissionSettings
	for key, value := range fields {
		if key != "allow" && key != "ask" && key != "deny" {
			return PermissionSettings{}, settingsError("unknown permissions key")
		}
		decoded, err := decodeSettingsStringList(value, "permissions."+key)
		if err != nil {
			return PermissionSettings{}, err
		}
		if len(decoded) != 0 {
			return PermissionSettings{}, settingsError("nonempty permission rules are unsupported")
		}
		switch key {
		case "allow":
			permissions.Allow = decoded
		case "ask":
			permissions.Ask = decoded
		case "deny":
			permissions.Deny = decoded
		}
	}
	return permissions, nil
}

func decodeSettingsStringList(raw json.RawMessage, field string) ([]string, error) {
	if isJSONNull(raw) {
		return nil, settingsError("null settings value")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, settingsFieldError(field)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(trimmed, &entries); err != nil {
		return nil, settingsFieldError(field)
	}
	values := make([]string, 0, len(entries))
	for _, entry := range entries {
		value, err := decodeSettingsString(entry, field)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}

func decodeSettingsString(raw json.RawMessage, field string) (string, error) {
	if isJSONNull(raw) {
		return "", settingsFieldError(field)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return "", settingsFieldError(field)
	}
	return value, nil
}

func decodeSettingsBool(raw json.RawMessage, field string) (bool, error) {
	if isJSONNull(raw) {
		return false, settingsFieldError(field)
	}
	trimmed := bytes.TrimSpace(raw)
	if !bytes.Equal(trimmed, []byte("true")) && !bytes.Equal(trimmed, []byte("false")) {
		return false, settingsFieldError(field)
	}
	var value bool
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return false, settingsFieldError(field)
	}
	return value, nil
}

func canonicalSettingsPath(value string) bool {
	return value != "" &&
		!strings.ContainsRune(value, '\x00') &&
		utf8.ValidString(value) &&
		filepath.IsAbs(value) &&
		filepath.Clean(value) == value
}

func isJSONNull(raw []byte) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

func settingsError(reason string) error {
	return fmt.Errorf("%w: %w: %s", ErrUnsupportedProfile, ErrInvalidSettings, reason)
}

func settingsFieldError(field string) error {
	return fmt.Errorf("%w: %w: invalid settings field %s", ErrUnsupportedProfile, ErrInvalidSettings, field)
}
