package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const claudePlaintextCredentialsFallbackKind = "claude-plaintext-credentials-fallback"

// observeClaudePlaintextCredentialsFallback deliberately uses Lstat only.
// Claude's native path checks Keychain first and falls back to
// ~/.claude/.credentials.json; the fallback is an OAuth credential store, so
// its contents must never be read, parsed, or hashed by policy inspection.
// An absent path is useful evidence. A present path is recorded as an opaque
// marker without opening it; the provider CLI remains the authority for the
// account actually used. This allows a normal signed-in account to retain
// Claude's native fallback file without copying or exposing it.
func observeClaudePlaintextCredentialsFallback(path string) (task.PolicySourceDigest, error) {
	result := claudeAbsentSource(path, claudePlaintextCredentialsFallbackKind)
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("%w: direct Claude credentials fallback metadata is unavailable", ErrUnsupportedProfile)
	}
	// Do not read, parse, or hash the credential bytes. The marker distinguishes
	// presence from absence while remaining independent of secret contents.
	result.Present = true
	result.SHA256 = task.ComputeSHA256([]byte("present"))
	return result, nil
}

type claudeJSONInspector func([]byte) ([]byte, error)

func observeClaudeJSONSource(source commonprovider.SourceBytes, kind string, inspector claudeJSONInspector) (task.PolicySourceDigest, error) {
	result := task.PolicySourceDigest{Path: source.Path, Kind: kind}
	if !source.Present {
		return result, nil
	}
	if source.Path == "" {
		return result, fmt.Errorf("%w: present Claude policy source has no path", ErrUnsupportedProfile)
	}
	shape, err := inspector(source.Data)
	if err != nil {
		return result, fmt.Errorf("%w: unsupported Claude policy source %s (%s): %w", ErrUnsupportedProfile, source.Path, kind, err)
	}
	// Inspectors return a key/type-only projection. Hashing the raw document
	// would hash values such as MCP headers or settings environment variables.
	result.Present = true
	result.SHA256 = task.ComputeSHA256(shape)
	return result, nil
}

func observeClaudeOpaqueEmptySource(source commonprovider.SourceBytes, kind string, allowNonEmpty bool) (task.PolicySourceDigest, error) {
	result := task.PolicySourceDigest{Path: source.Path, Kind: kind}
	if !source.Present {
		return result, nil
	}
	if source.Path == "" {
		return result, fmt.Errorf("%w: present Claude policy source has no path", ErrUnsupportedProfile)
	}
	if len(bytes.TrimSpace(source.Data)) != 0 {
		if allowNonEmpty {
			return result, fmt.Errorf("%w: present remote consent source %s cannot be verified by the static resolver", ErrUnsupportedProfile, source.Path)
		}
		return result, fmt.Errorf("%w: present remote cache integrity source %s cannot be verified by the static resolver", ErrUnsupportedProfile, source.Path)
	}
	result.Present = true
	result.SHA256 = task.ComputeSHA256([]byte("present-empty\n"))
	return result, nil
}

func observeOpaqueManagedSource(source commonprovider.SourceBytes, kind string) (task.PolicySourceDigest, error) {
	result := task.PolicySourceDigest{Path: source.Path, Kind: kind}
	if !source.Present {
		return result, nil
	}
	return result, fmt.Errorf("%w: present managed Claude source %s (%s) cannot be parsed faithfully by the static resolver", ErrUnsupportedProfile, source.Path, kind)
}

func inspectClaudeGlobalConfig(data []byte) ([]byte, error) {
	root, err := decodeClaudeJSONObject(data)
	if err != nil {
		return nil, err
	}
	activeRoot := claudeGlobalActiveView(root)
	if err := rejectClaudeGlobalSecrets(activeRoot); err != nil {
		return nil, err
	}
	if err := rejectClaudeGlobalNativeState(activeRoot); err != nil {
		return nil, err
	}
	return claudeJSONShape(root), nil
}

// claudeGlobalActiveView removes native bookkeeping and feature-cache
// subtrees before running the policy checks. These maps are persisted in the
// global document, but native consumers use them only for telemetry and
// experiment evaluation; records may therefore contain names that collide
// with active policy keys such as allowedTools or statusLine. Keeping this
// projection separate from the original root preserves the complete key/type
// shape digest while ensuring every active-state check applies the same scope
// rule. A similarly named field nested under an active object remains visible
// and is checked normally.
var claudeGlobalInactiveRootKeys = map[string]struct{}{
	"skillUsage":                 {},
	"cachedGrowthBookFeatures":   {},
	"cachedGrowthBookFeaturesAt": {},
	"cachedExperimentFeatures":   {},
	"cachedExperimentData":       {},
}

func claudeGlobalActiveView(root map[string]any) map[string]any {
	remove := false
	for key := range root {
		if _, inactive := claudeGlobalInactiveRootKeys[key]; inactive {
			remove = true
			break
		}
	}
	if !remove {
		return root
	}
	active := make(map[string]any, len(root)-len(claudeGlobalInactiveRootKeys))
	for key, value := range root {
		if _, inactive := claudeGlobalInactiveRootKeys[key]; inactive {
			continue
		}
		active[key] = value
	}
	return active
}

func inspectClaudeSettings(data []byte) ([]byte, error) {
	root, err := decodeClaudeJSONObject(data)
	if err != nil {
		return nil, err
	}
	// The restricted launch passes --safe-mode and --restricted and supplies
	// task-owned settings, so user/project settings are inactive. Keep only a
	// key/type shape for drift evidence while allowing native UX fields to evolve.
	return claudeJSONShape(root), nil
}

func inspectClaudeMCP(data []byte) ([]byte, error) {
	root, err := decodeClaudeJSONObject(data)
	if err != nil {
		return nil, err
	}
	for key, value := range root {
		if key != "mcpServers" {
			return nil, fmt.Errorf("unrecognized .mcp.json field %q", key)
		}
		if _, ok := value.(map[string]any); !ok {
			return nil, errors.New("mcpServers must be an object")
		}
	}
	return claudeJSONShape(root), nil
}

func inspectClaudeRemoteSettings(data []byte) ([]byte, error) {
	root, err := decodeClaudeJSONObject(data)
	if err != nil {
		return nil, err
	}
	if len(root) == 0 {
		return claudeJSONShape(root), nil
	}
	for key := range root {
		// The native cache projection preserves the JSON-schema marker while
		// dropping other dollar-prefixed metadata. It carries no executable or
		// policy value and is compatible with an empty snapshot.
		if key != "$schema" {
			return nil, errors.New("remote settings cache contains a non-empty policy snapshot")
		}
	}
	return claudeJSONShape(root), nil
}

func inspectClaudeRemoteConsentRecords(data []byte) ([]byte, error) {
	root, err := decodeClaudeJSONObject(data)
	if err != nil {
		return nil, err
	}
	if len(root) != 2 {
		return nil, errors.New("remote consent records have an unsupported shape")
	}
	version, ok := root["version"].(json.Number)
	if !ok || version.String() != "1" {
		return nil, errors.New("remote consent records have an unsupported version")
	}
	records, ok := root["records"].(map[string]any)
	if !ok {
		return nil, errors.New("remote consent records are not an object")
	}
	for _, record := range records {
		fields, ok := record.(map[string]any)
		if !ok || len(fields) != 3 {
			return nil, errors.New("remote consent record has an unsupported shape")
		}
		for _, key := range []string{"accountUuid", "dangerousSettingsHash", "updatedAt"} {
			if _, present := fields[key]; !present {
				return nil, errors.New("remote consent record has an unsupported shape")
			}
		}
		if _, ok := fields["accountUuid"].(string); !ok {
			return nil, errors.New("remote consent record account identity is invalid")
		}
		if _, ok := fields["dangerousSettingsHash"].(string); !ok {
			return nil, errors.New("remote consent record hash is invalid")
		}
		if _, ok := fields["updatedAt"].(json.Number); !ok {
			return nil, errors.New("remote consent record timestamp is invalid")
		}
	}
	return claudeJSONShape(root), nil
}

func decodeClaudeJSONObject(data []byte) (map[string]any, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("source is not valid UTF-8")
	}
	if err := task.ValidateJSONStructure(data); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode source JSON: %w", err)
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("source root must be an object")
	}
	return root, nil
}

func rejectClaudeGlobalSecrets(value any) error {
	return walkClaudeJSONKeysIgnoringOrdinaryMCP(value, func(key string, _ []string) error {
		switch strings.ToLower(key) {
		case "primaryapikey":
			// Do not include the value in an error and do not compute a digest
			// before this check. Native primaryApiKey is an alternate auth path.
			return errors.New("primaryApiKey is present")
		case "apikeyhelper":
			return errors.New("apiKeyHelper executable is present")
		default:
			return nil
		}
	})
}

// The native global config is also the persistence target for GrowthBook and
// experiment evaluation. The prepared environment pins the native
// StorageV5=false latch before this cache is consulted, so these records are
// shape-only policy observations here; their values never enter a digest or
// an error. Executable, alternate-auth, and active MCP controls below remain
// unsupported regardless of the pin.

var claudeGlobalMCPObjectKeys = map[string]struct{}{
	"mcpservers":        {},
	"managedmcpservers": {},
}

var claudeGlobalMCPListKeys = map[string]struct{}{
	"allowedmcpservers":      {},
	"deniedmcpservers":       {},
	"enabledmcpjsonservers":  {},
	"disabledmcpjsonservers": {},
	"allowedtools":           {},
	"disallowedtools":        {},
}

// Global settings can carry credential helpers and user-scoped MCP/plugin
// definitions in addition to the dedicated settings files. They are active
// inputs for native auth/config resolution, so a static resolver cannot
// safely project them from a shape-only digest. Presence is refused before
// values can be hashed.
var claudeGlobalExecutableOrMCPKeys = map[string]struct{}{
	"awsauthrefresh":         {},
	"awscredentialexport":    {},
	"gcpauthrefresh":         {},
	"otelheadershelper":      {},
	"proxyauthhelper":        {},
	"hooks":                  {},
	"statusline":             {},
	"filesuggestion":         {},
	"lspservers":             {},
	"enabledplugins":         {},
	"pluginconfigs":          {},
	"extraknownmarketplaces": {},
	"policyhelpers":          {},
}

func rejectClaudeGlobalNativeState(value any) error {
	if err := rejectClaudeGlobalEmptyControls(value); err != nil {
		return err
	}
	return walkClaudeJSONKeysIgnoringOrdinaryMCP(value, func(key string, _ []string) error {
		if _, found := claudeGlobalExecutableOrMCPKeys[strings.ToLower(key)]; found {
			return errors.New("global executable or MCP policy source is unsupported")
		}
		return nil
	})
}

// Global project records normally carry empty MCP/tool collections. Empty
// collections are harmless and their key/type shape is included in the
// digest. A non-empty collection is an active policy input; its values cannot
// be projected without either hashing commands/URLs or depending on native
// schema semantics, so refuse it before digesting the source.
func rejectClaudeGlobalEmptyControls(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		return rejectClaudeGlobalObjectControls(typed)
	case []any:
		return rejectClaudeGlobalArrayControls(typed)
	default:
		return nil
	}
}

func rejectClaudeGlobalObjectControls(object map[string]any) error {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		lowerKey := strings.ToLower(key)
		value := object[key]
		if key == "mcpServers" {
			continue
		}
		if _, found := claudeGlobalMCPObjectKeys[lowerKey]; found && !claudeEmptyObject(value) {
			return errors.New("non-empty global MCP configuration is unsupported")
		}
		if _, found := claudeGlobalMCPListKeys[lowerKey]; found && !claudeEmptyArray(value) {
			return errors.New("non-empty global tool or MCP policy is unsupported")
		}
		if err := rejectClaudeGlobalEmptyControls(value); err != nil {
			return err
		}
	}
	return nil
}

func rejectClaudeGlobalArrayControls(array []any) error {
	for _, value := range array {
		if err := rejectClaudeGlobalEmptyControls(value); err != nil {
			return err
		}
	}
	return nil
}

func claudeEmptyObject(value any) bool {
	object, ok := value.(map[string]any)
	return ok && len(object) == 0
}

func claudeEmptyArray(value any) bool {
	array, ok := value.([]any)
	return ok && len(array) == 0
}

// walkClaudeJSONKeysIgnoringOrdinaryMCP walks the global configuration while
// omitting each ordinary mcpServers subtree. The fixed strict profile owns
// the MCP config it loads, so server commands, env, and headers in these
// native ordinary MCP maps are not active inputs for this invocation. Root
// skillUsage bookkeeping is projected out once by claudeGlobalActiveView;
// nested or differently-cased fields remain visible to the normal checks.
func walkClaudeJSONKeysIgnoringOrdinaryMCP(value any, visit func(string, []string) error) error {
	return walkClaudeJSONValueIgnoringOrdinaryMCP(value, nil, visit)
}

func walkClaudeJSONValueIgnoringOrdinaryMCP(value any, path []string, visit func(string, []string) error) error {
	switch typed := value.(type) {
	case map[string]any:
		return walkClaudeJSONObjectIgnoringOrdinaryMCP(typed, path, visit)
	case []any:
		return walkClaudeJSONArrayIgnoringOrdinaryMCP(typed, path, visit)
	}
	return nil
}

func walkClaudeJSONObjectIgnoringOrdinaryMCP(object map[string]any, path []string, visit func(string, []string) error) error {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if key == "mcpServers" {
			continue
		}
		if err := visit(key, path); err != nil {
			return err
		}
		if err := walkClaudeJSONValueIgnoringOrdinaryMCP(object[key], append(path, key), visit); err != nil {
			return err
		}
	}
	return nil
}

func walkClaudeJSONArrayIgnoringOrdinaryMCP(array []any, path []string, visit func(string, []string) error) error {
	for index, item := range array {
		if err := walkClaudeJSONValueIgnoringOrdinaryMCP(item, append(path, fmt.Sprintf("[%d]", index)), visit); err != nil {
			return err
		}
	}
	return nil
}

type claudeJSONShapeEntry struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Length int    `json:"length,omitempty"`
}

func claudeJSONShape(value any) []byte {
	entries := make([]claudeJSONShapeEntry, 0)
	var walk func(any, string)
	walk = func(current any, path string) {
		switch typed := current.(type) {
		case map[string]any:
			entries = append(entries, claudeJSONShapeEntry{Path: path, Type: "object", Length: len(typed)})
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			for _, key := range keys {
				child := key
				if path != "" {
					child = path + "." + key
				}
				walk(typed[key], child)
			}
		case []any:
			entries = append(entries, claudeJSONShapeEntry{Path: path, Type: "array", Length: len(typed)})
			for index, item := range typed {
				walk(item, fmt.Sprintf("%s[%d]", path, index))
			}
		case nil:
			entries = append(entries, claudeJSONShapeEntry{Path: path, Type: "null"})
		case string:
			entries = append(entries, claudeJSONShapeEntry{Path: path, Type: "string"})
		case bool:
			entries = append(entries, claudeJSONShapeEntry{Path: path, Type: "bool"})
		case json.Number:
			entries = append(entries, claudeJSONShapeEntry{Path: path, Type: "number"})
		default:
			entries = append(entries, claudeJSONShapeEntry{Path: path, Type: fmt.Sprintf("%T", current)})
		}
	}
	walk(value, "")
	encoded, err := json.Marshal(entries)
	if err != nil {
		// All values are local fixed-shape records; this cannot fail. Keep the
		// digest input deterministic even if a future type is added.
		return []byte("[]")
	}
	return append(encoded, '\n')
}
