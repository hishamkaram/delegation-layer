package pi

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// effectivePolicy records the caller-selected native Pi mode, runtime roots,
// and nonsecret native source observations. It intentionally contains no
// release claim or credential material.
func effectivePolicy(request task.TaskRecord, environment profileEnvironment, runtimeSHA256 string, sources []task.PolicySourceDigest) (task.EffectiveConfig, error) {
	if request.Mode != ModeReadOnly || request.CanonicalCwd == "" || task.ValidateSHA256(runtimeSHA256) != nil {
		return task.EffectiveConfig{}, fmt.Errorf("%w: invalid Pi policy inputs", ErrUnsupportedProfile)
	}
	sources = slices.Clone(sources)
	slices.SortFunc(sources, func(left, right task.PolicySourceDigest) int {
		if order := cmp.Compare(left.Path, right.Path); order != 0 {
			return order
		}
		return cmp.Compare(left.Kind, right.Kind)
	})
	details := &task.PolicyDetails{
		ProfileRevision: ProfileRevision,
		RuntimeSHA256:   runtimeSHA256,
		Workspace:       request.CanonicalCwd,
		WritableRoots:   slices.Clone(environment.WritableRoots),
		Sources:         sources,
	}
	effective := task.EffectiveConfig{
		Containment: request.Mode,
		Approval:    "caller-selected",
		Policy:      details,
	}
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

const (
	piModelsConfigKind          = "models-config"
	piAuthConfigKind            = "auth-config"
	piGlobalSettingsConfigKind  = "global-settings-config"
	piProjectSettingsConfigKind = "project-settings-config"
)

type piAuthProjection struct {
	Provider   string `json:"provider"`
	Type       string `json:"type"`
	KeyCommand bool   `json:"key_command"`
}

// inspectModelsConfig observes Pi's model configuration without resolving
// credentials. Pi treats values beginning with "!" as shell commands when it
// resolves API keys and headers, so those values are rejected before the
// profile is admitted. The persisted digest is calculated over a
// credential-blind projection: known model topology and endpoint fields are
// retained, while API keys, header values, and unknown string values are
// represented only by their type/presence. The ordinary preflight rebuild
// therefore detects policy-relevant changes without persisting secrets or a
// digest of their bytes.
func inspectModelsConfig(agentDir string) (task.PolicySourceDigest, error) {
	path := filepath.Join(agentDir, "models.json")
	source, err := commonprovider.ReadPolicySource(path)
	result := task.PolicySourceDigest{Path: path, Kind: piModelsConfigKind, Present: source.Present}
	if err != nil {
		return task.PolicySourceDigest{}, fmt.Errorf("%w: inspect Pi models config: %w", ErrUnsupportedProfile, err)
	}
	if !source.Present {
		return result, nil
	}
	if structureErr := task.ValidateJSONStructure(source.Data); structureErr != nil {
		return task.PolicySourceDigest{}, fmt.Errorf("%w: invalid Pi models config: %w", ErrUnsupportedProfile, structureErr)
	}
	var value any
	if decodeErr := json.Unmarshal(source.Data, &value); decodeErr != nil {
		return task.PolicySourceDigest{}, fmt.Errorf("%w: decode Pi models config: %w", ErrUnsupportedProfile, decodeErr)
	}
	if rejectErr := rejectCommandConfigValues(value, "models.json"); rejectErr != nil {
		return task.PolicySourceDigest{}, fmt.Errorf("%w: %w", ErrUnsupportedProfile, rejectErr)
	}
	projection := projectModelConfigValue(value, "", false, false)
	encoded, err := task.MarshalCanonical(projection)
	if err != nil {
		return task.PolicySourceDigest{}, fmt.Errorf("%w: project Pi models config: %w", ErrUnsupportedProfile, err)
	}
	result.SHA256 = task.ComputeSHA256(encoded)
	return result, nil
}

// inspectAuthConfig checks only the credential shape and command marker. Pi
// resolves an API-key value beginning with "!" by executing it as a shell
// command, including when that value comes from auth.json. The projection
// records provider/type/command presence only; OAuth and API-key bytes are
// never hashed or persisted. Its digest is nevertheless bound into the
// effective policy so a fresh preflight notices source changes.
func inspectAuthConfig(agentDir string) (task.PolicySourceDigest, error) {
	path := filepath.Join(agentDir, "auth.json")
	source, err := commonprovider.ReadPolicySource(path)
	result := task.PolicySourceDigest{Path: path, Kind: piAuthConfigKind, Present: source.Present}
	if err != nil {
		return task.PolicySourceDigest{}, fmt.Errorf("%w: inspect Pi authentication config: %w", ErrUnsupportedProfile, err)
	}
	if !source.Present {
		return result, nil
	}
	projection, projectionErr := projectAuthConfig(source.Data)
	if projectionErr != nil {
		return task.PolicySourceDigest{}, projectionErr
	}
	encoded, err := task.MarshalCanonical(projection)
	if err != nil {
		return task.PolicySourceDigest{}, err
	}
	result.SHA256 = task.ComputeSHA256(encoded)
	return result, nil
}

func projectAuthConfig(data []byte) ([]piAuthProjection, error) {
	if err := task.ValidateJSONStructure(data); err != nil {
		return nil, fmt.Errorf("%w: invalid Pi authentication config: %w", ErrUnsupportedProfile, err)
	}
	var entries map[string]json.RawMessage
	if decodeErr := json.Unmarshal(data, &entries); decodeErr != nil || entries == nil {
		return nil, fmt.Errorf("%w: Pi authentication config must be an object", ErrUnsupportedProfile)
	}
	projection := make([]piAuthProjection, 0, len(entries))
	for provider, raw := range entries {
		entry, entryErr := projectAuthEntry(raw)
		if entryErr != nil {
			return nil, entryErr
		}
		entry.Provider = provider
		projection = append(projection, entry)
	}
	slices.SortFunc(projection, func(left, right piAuthProjection) int { return cmp.Compare(left.Provider, right.Provider) })
	return projection, nil
}

func projectAuthEntry(raw json.RawMessage) (piAuthProjection, error) {
	var entry map[string]json.RawMessage
	if decodeErr := json.Unmarshal(raw, &entry); decodeErr != nil || entry == nil {
		return piAuthProjection{}, fmt.Errorf("%w: Pi authentication entry is not an object", ErrUnsupportedProfile)
	}
	typeRaw, ok := entry["type"]
	if !ok {
		return piAuthProjection{}, fmt.Errorf("%w: Pi authentication entry has no type", ErrUnsupportedProfile)
	}
	var credentialType string
	if decodeErr := json.Unmarshal(typeRaw, &credentialType); decodeErr != nil || credentialType == "" {
		return piAuthProjection{}, fmt.Errorf("%w: Pi authentication entry has an invalid type", ErrUnsupportedProfile)
	}
	if keyRaw, exists := entry["key"]; exists {
		var key string
		if decodeErr := json.Unmarshal(keyRaw, &key); decodeErr != nil {
			return piAuthProjection{}, fmt.Errorf("%w: Pi authentication key has an invalid shape", ErrUnsupportedProfile)
		}
		if strings.HasPrefix(key, "!") {
			return piAuthProjection{}, fmt.Errorf("%w: Pi authentication config contains a command-valued API key", ErrUnsupportedProfile)
		}
	}
	return piAuthProjection{Type: credentialType}, nil
}

// inspectSettingsConfig records the credential-blind settings that Pi merges
// before selecting a provider, model, thinking level, retry policy, and
// runtime behavior. The adapter passes explicit values when the caller has
// selected them, but defaults remain effective for fresh tasks; both the
// global and project files therefore belong to the queued-task policy
// snapshot. Secret-bearing keys retain only their shape and presence.
func inspectSettingsConfig(path, kind string) (task.PolicySourceDigest, error) {
	source, err := commonprovider.ReadPolicySource(path)
	result := task.PolicySourceDigest{Path: path, Kind: kind, Present: source.Present}
	if err != nil {
		return task.PolicySourceDigest{}, fmt.Errorf("%w: inspect Pi settings config: %w", ErrUnsupportedProfile, err)
	}
	if !source.Present {
		return result, nil
	}
	if structureErr := task.ValidateJSONStructure(source.Data); structureErr != nil {
		return task.PolicySourceDigest{}, fmt.Errorf("%w: invalid Pi settings config: %w", ErrUnsupportedProfile, structureErr)
	}
	decoder := json.NewDecoder(bytes.NewReader(source.Data))
	decoder.UseNumber()
	var value any
	if decodeErr := decoder.Decode(&value); decodeErr != nil {
		return task.PolicySourceDigest{}, fmt.Errorf("%w: decode Pi settings config: %w", ErrUnsupportedProfile, decodeErr)
	}
	if _, ok := value.(map[string]any); !ok {
		return task.PolicySourceDigest{}, fmt.Errorf("%w: Pi settings config must be an object", ErrUnsupportedProfile)
	}
	projection := projectSettingsValue(value, "")
	encoded, err := task.MarshalCanonical(projection)
	if err != nil {
		return task.PolicySourceDigest{}, fmt.Errorf("%w: project Pi settings config: %w", ErrUnsupportedProfile, err)
	}
	result.SHA256 = task.ComputeSHA256(encoded)
	return result, nil
}

func inspectPolicySources(agentDir, workspace string) ([]task.PolicySourceDigest, error) {
	models, err := inspectModelsConfig(agentDir)
	if err != nil {
		return nil, err
	}
	auth, err := inspectAuthConfig(agentDir)
	if err != nil {
		return nil, err
	}
	globalSettings, err := inspectSettingsConfig(filepath.Join(agentDir, "settings.json"), piGlobalSettingsConfigKind)
	if err != nil {
		return nil, err
	}
	projectSettings, err := inspectSettingsConfig(filepath.Join(workspace, ".pi", "settings.json"), piProjectSettingsConfigKind)
	if err != nil {
		return nil, err
	}
	sources := []task.PolicySourceDigest{models, auth, globalSettings, projectSettings}
	slices.SortFunc(sources, func(left, right task.PolicySourceDigest) int {
		if order := cmp.Compare(left.Path, right.Path); order != 0 {
			return order
		}
		return cmp.Compare(left.Kind, right.Kind)
	})
	return sources, nil
}

func rejectCommandConfigValues(value any, path string) error {
	switch typed := value.(type) {
	case string:
		if strings.HasPrefix(typed, "!") {
			return fmt.Errorf("pi models config contains a command-valued setting at %s", path)
		}
	case []any:
		for index, child := range typed {
			if err := rejectCommandConfigValues(child, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	case map[string]any:
		for key, child := range typed {
			if err := rejectCommandConfigValues(child, path+"."+key); err != nil {
				return err
			}
		}
	}
	return nil
}

// projectModelConfigValue returns the nonsecret model-policy observation used
// in the effective configuration. Safe schema fields retain their values so
// endpoint/model changes are noticed; credential-bearing fields and unknown
// strings retain only shape and presence. `redact` is inherited by header and
// credential objects, while `safe` is inherited by schema subobjects whose
// string values are known to be policy metadata.
func projectModelConfigValue(value any, key string, redact, safe bool) any {
	if modelConfigSensitiveKey(key) || strings.EqualFold(key, "headers") {
		redact = true
	}
	if modelConfigSafeMapKey(key) {
		safe = true
	}
	return projectModelValue(value, key, redact, safe)
}

func projectModelValue(value any, key string, redact, safe bool) any {
	switch typed := value.(type) {
	case nil:
		if redact {
			return map[string]any{"type": "null"}
		}
		return nil
	case string:
		if redact {
			return map[string]any{"configured": true, "type": "string"}
		}
		if safe || modelConfigSafeStringKey(key) {
			return typed
		}
		return map[string]any{"type": "string"}
	case bool:
		if redact {
			return map[string]any{"type": "boolean"}
		}
		return typed
	case float64:
		if redact {
			return map[string]any{"type": "number"}
		}
		return typed
	case []any:
		return projectModelArray(typed, key, redact, safe)
	case map[string]any:
		return projectModelMap(typed, redact, safe)
	default:
		return map[string]any{"type": fmt.Sprintf("%T", typed)}
	}
}

func projectModelArray(values []any, key string, redact, safe bool) []any {
	projected := make([]any, len(values))
	for index, child := range values {
		projected[index] = projectModelConfigValue(child, key, redact, safe)
	}
	return projected
}

func projectModelMap(values map[string]any, redact, safe bool) map[string]any {
	projected := make(map[string]any, len(values))
	for childKey, child := range values {
		projected[childKey] = projectModelConfigValue(child, childKey, redact, safe)
	}
	return projected
}

func modelConfigSensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
	for _, marker := range []string{"apikey", "token", "secret", "password", "credential", "authorization"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func modelConfigSafeStringKey(key string) bool {
	switch strings.ToLower(key) {
	case "id", "name", "api", "baseurl", "oauth", "type", "provider", "model", "text", "image":
		return true
	default:
		return false
	}
}

func modelConfigSafeMapKey(key string) bool {
	switch strings.ToLower(key) {
	case "thinkinglevelmap", "compat", "openrouterrouting", "vercelgatewayrouting", "input":
		return true
	default:
		return false
	}
}

func projectSettingsValue(value any, key string) any {
	if settingsSensitiveKey(key) {
		return map[string]any{"configured": value != nil, "type": settingsValueType(value)}
	}
	switch typed := value.(type) {
	case map[string]any:
		projected := make(map[string]any, len(typed))
		for childKey, childValue := range typed {
			projected[childKey] = projectSettingsValue(childValue, childKey)
		}
		return projected
	case []any:
		projected := make([]any, len(typed))
		for index, childValue := range typed {
			projected[index] = projectSettingsValue(childValue, key)
		}
		return projected
	default:
		return value
	}
}

func settingsSensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
	for _, marker := range []string{"apikey", "accesskey", "token", "secret", "password", "credential", "authorization", "cookie", "headers", "env"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func settingsValueType(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number, float64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "value"
	}
}
