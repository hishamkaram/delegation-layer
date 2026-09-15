package claude

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// Claude Code resolves these machine-wide paths on macOS. The managed
// plist paths are intentionally kept as whole files: Claude reads their full
// dictionaries through plutil, so a CFPreferences lookup is not equivalent.
const (
	claudeManagedRoot        = "/Library/Application Support/ClaudeCode"
	claudeManagedPreferences = "/Library/Managed Preferences"
)

const (
	claudeGlobalConfigKind         = "claude-global-config"
	claudeLegacyGlobalConfigKind   = "claude-global-legacy-config"
	claudeUserSettingsKind         = "claude-user-settings"
	claudeProjectSettingsKind      = "claude-project-settings"
	claudeLocalSettingsKind        = "claude-local-settings"
	claudeProjectMCPKind           = "claude-project-mcp"
	claudeManagedSettingsKind      = "claude-managed-settings"
	claudeManagedDropInKind        = "claude-managed-settings-drop-in"
	claudeManagedMCPKind           = "claude-managed-mcp"
	claudeManagedUserPlistKind     = "claude-managed-user-plist"
	claudeManagedDevicePlistKind   = "claude-managed-device-plist"
	claudeRemoteSettingsKind       = "claude-remote-settings-cache"
	claudeRemoteConsentKind        = "claude-remote-settings-consent"
	claudeRemoteConsentRecordsKind = "claude-remote-settings-consent-records"
	claudeRemoteSignatureKind      = "claude-remote-settings-signature"
	claudeRemoteIATKind            = "claude-remote-settings-signature-iat"
)

// claudePolicySourceReader is a narrow seam for tests. The production reader
// is commonprovider.ReadPolicySource, which performs canonical-path, regular
// file, no-follow, bounded, and stable-read checks. No source bytes cross the
// resolver boundary after the non-secret observation is made.
type claudePolicySourceReader func(string) (commonprovider.SourceBytes, error)

// resolvePolicySources inventories the policy files that can affect the
// fixed restricted profile. User/project/local settings remain observed
// because their presence is part of the admission evidence. The strict MCP
// argv bypasses ordinary user/project/local MCP server loading;
// managed settings, managed MCP, and executable/auth controls remain active.
//
// The resolver never reads Keychain or credentials files. It records only the
// metadata-only presence or absence of the direct plaintext OAuth fallback
// used after the native Keychain lookup. Native inspection separately proves
// the signed-in Keychain account before launch. A present managed plist/managed
// JSON source is refused because this pure resolver cannot faithfully parse the
// native whole-document policy semantics.
func resolvePolicySources(request task.TaskRecord, environment profileEnvironment) ([]task.PolicySourceDigest, error) {
	username, err := currentClaudeOSUsername()
	if err != nil {
		return nil, err
	}
	return resolvePolicySourcesWithReaderAndUsername(request, environment, commonprovider.ReadPolicySource, username)
}

func resolvePolicySourcesWithReaderAndUsername(
	request task.TaskRecord,
	environment profileEnvironment,
	reader claudePolicySourceReader,
	username string,
) ([]task.PolicySourceDigest, error) {
	if reader == nil {
		return nil, fmt.Errorf("%w: nil policy source reader", ErrUnsupportedProfile)
	}
	if err := validatePolicySourceInputs(request, environment, username); err != nil {
		return nil, err
	}
	collector := claudePolicySourceCollector{reader: reader}
	if err := collector.addGlobalSources(environment.Home, environment.ClaudeHome); err != nil {
		return nil, err
	}
	if err := collector.addPlaintextCredentialsFallback(environment.ClaudeHome); err != nil {
		return nil, err
	}
	if err := collector.addUserSources(environment.ClaudeHome); err != nil {
		return nil, err
	}
	if err := collector.addRemoteSources(environment.ClaudeHome); err != nil {
		return nil, err
	}
	ancestors, err := claudeWorkspaceAncestors(request.CanonicalCwd)
	if err != nil {
		return nil, err
	}
	if err := collector.addWorkspaceSources(ancestors, environment.Home); err != nil {
		return nil, err
	}
	if err := collector.addManagedSources(username); err != nil {
		return nil, err
	}
	collector.sort()
	return collector.sources, nil
}

type claudePolicySourceCollector struct {
	reader  claudePolicySourceReader
	sources []task.PolicySourceDigest
}

func (collector *claudePolicySourceCollector) add(source task.PolicySourceDigest, err error) error {
	if err != nil {
		return err
	}
	collector.sources = append(collector.sources, source)
	if len(collector.sources) > task.MaxPolicySources {
		return fmt.Errorf("%w: policy source inventory exceeds the source bound", ErrUnsupportedProfile)
	}
	return nil
}

func (collector *claudePolicySourceCollector) addJSON(path, kind string, inspector claudeJSONInspector) error {
	source, err := readClaudeSource(collector.reader, path)
	if err != nil {
		return err
	}
	observed, err := observeClaudeJSONSource(source, kind, inspector)
	return collector.add(observed, err)
}

func (collector *claudePolicySourceCollector) addOpaqueEmpty(path, kind string, allowNonEmpty bool) error {
	source, err := readClaudeSource(collector.reader, path)
	if err != nil {
		return err
	}
	observed, err := observeClaudeOpaqueEmptySource(source, kind, allowNonEmpty)
	return collector.add(observed, err)
}

func (collector *claudePolicySourceCollector) addManaged(path, kind string) error {
	source, err := readClaudeSource(collector.reader, path)
	if err != nil {
		return err
	}
	observed, err := observeOpaqueManagedSource(source, kind)
	return collector.add(observed, err)
}

func (collector *claudePolicySourceCollector) addPlaintextCredentialsFallback(claudeHome string) error {
	path := filepath.Join(claudeHome, ".credentials.json")
	observed, err := observeClaudePlaintextCredentialsFallback(path)
	return collector.add(observed, err)
}

func (collector *claudePolicySourceCollector) addGlobalSources(home, claudeHome string) error {
	// Native Claude chooses the legacy file inside ~/.claude whenever it is
	// present, and only then falls back to ~/.claude.json. Do not inspect or
	// hash the inactive fallback: it is not an input to this invocation, and a
	// fallback with primaryApiKey must not defeat the documented precedence.
	legacyPath := filepath.Join(claudeHome, ".config.json")
	legacy, err := readClaudeSource(collector.reader, legacyPath)
	if err != nil {
		return err
	}
	if legacy.Present {
		observed, observeErr := observeClaudeJSONSource(legacy, claudeLegacyGlobalConfigKind, inspectClaudeGlobalConfig)
		return collector.add(observed, observeErr)
	}
	if err := collector.add(claudeAbsentSource(legacyPath, claudeLegacyGlobalConfigKind), nil); err != nil {
		return err
	}
	return collector.addJSON(filepath.Join(home, ".claude.json"), claudeGlobalConfigKind, inspectClaudeGlobalConfig)
}

func (collector *claudePolicySourceCollector) addUserSources(claudeHome string) error {
	return collector.addJSON(filepath.Join(claudeHome, "settings.json"), claudeUserSettingsKind, inspectClaudeSettings)
}

func (collector *claudePolicySourceCollector) addRemoteSources(claudeHome string) error {
	remoteSettingsPath := filepath.Join(claudeHome, "remote-settings.json")
	if err := collector.addJSON(remoteSettingsPath, claudeRemoteSettingsKind, inspectClaudeRemoteSettings); err != nil {
		return err
	}
	if err := collector.addOpaqueEmpty(filepath.Join(claudeHome, "remote-settings-helper-consent"), claudeRemoteConsentKind, true); err != nil {
		return err
	}
	if err := collector.addJSON(filepath.Join(claudeHome, "remote-settings-consent.json"), claudeRemoteConsentRecordsKind, inspectClaudeRemoteConsentRecords); err != nil {
		return err
	}
	for _, signature := range []struct {
		suffix string
		kind   string
	}{
		{suffix: ".signature.json", kind: claudeRemoteSignatureKind},
		{suffix: ".signature-iat.json", kind: claudeRemoteIATKind},
	} {
		if err := collector.addOpaqueEmpty(remoteSettingsPath+signature.suffix, signature.kind, false); err != nil {
			return err
		}
	}
	return nil
}

func (collector *claudePolicySourceCollector) addWorkspaceSources(ancestors []string, home string) error {
	if err := collector.addWorkspaceSettings(ancestors, home); err != nil {
		return err
	}
	// Strict MCP mode selects only the task-owned empty --mcp-config input. We
	// still inventory every native project .mcp.json candidate so its shape is
	// covered by the policy digest, while its server values are deliberately
	// ignored by the restricted execution profile.
	for _, directory := range ancestors {
		if err := collector.addJSON(filepath.Join(directory, ".mcp.json"), claudeProjectMCPKind, inspectClaudeMCP); err != nil {
			return err
		}
	}
	return nil
}

func (collector *claudePolicySourceCollector) addWorkspaceSettings(ancestors []string, home string) error {
	for _, directory := range ancestors {
		// ~/.claude is the user settings directory, not a project layer. A
		// checkout rooted at HOME therefore has no duplicate project source.
		if directory == home {
			continue
		}
		if err := collector.addJSON(filepath.Join(directory, ".claude", "settings.json"), claudeProjectSettingsKind, inspectClaudeSettings); err != nil {
			return err
		}
		if err := collector.addJSON(filepath.Join(directory, ".claude", "settings.local.json"), claudeLocalSettingsKind, inspectClaudeSettings); err != nil {
			return err
		}
	}
	return nil
}

func (collector *claudePolicySourceCollector) addManagedSources(username string) error {
	managedSources := []struct {
		path string
		kind string
	}{
		{filepath.Join(claudeManagedRoot, "managed-settings.json"), claudeManagedSettingsKind},
		{filepath.Join(claudeManagedRoot, "managed-settings.d"), claudeManagedDropInKind},
		{filepath.Join(claudeManagedRoot, "managed-mcp.json"), claudeManagedMCPKind},
		{filepath.Join(claudeManagedPreferences, username, "com.anthropic.claudecode.plist"), claudeManagedUserPlistKind},
		{filepath.Join(claudeManagedPreferences, "com.anthropic.claudecode.plist"), claudeManagedDevicePlistKind},
	}
	for _, managed := range managedSources {
		if err := collector.addManaged(managed.path, managed.kind); err != nil {
			return err
		}
	}
	return nil
}

func (collector *claudePolicySourceCollector) sort() {
	slices.SortFunc(collector.sources, func(left, right task.PolicySourceDigest) int {
		if order := cmp.Compare(left.Path, right.Path); order != 0 {
			return order
		}
		return cmp.Compare(left.Kind, right.Kind)
	})
}

func validatePolicySourceInputs(request task.TaskRecord, environment profileEnvironment, username string) error {
	if err := validateRequest(request); err != nil {
		return err
	}
	if err := validateMCPArguments(request); err != nil {
		return err
	}
	for name, path := range map[string]string{"home": environment.Home, "Claude home": environment.ClaudeHome} {
		if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, '\x00') {
			return fmt.Errorf("%w: %s must be a clean absolute path", ErrUnsupportedProfile, name)
		}
	}
	if filepath.Join(environment.Home, ".claude") != environment.ClaudeHome {
		return fmt.Errorf("%w: Claude home must be HOME/.claude", ErrUnsupportedProfile)
	}
	entries, err := parseEnvironment(environment.Values)
	if err != nil {
		return err
	}
	if entries["CLAUDE_CODE_HOVER_REST"] != "0" {
		return fmt.Errorf("%w: fixed StorageV5 false pin is required", ErrUnsupportedProfile)
	}
	if username == "" || filepath.Base(username) != username || strings.ContainsRune(username, '\x00') {
		return fmt.Errorf("%w: native managed-policy username is unavailable", ErrUnsupportedProfile)
	}
	return nil
}

// validateMCPArguments binds the source projection below to the
// exact read-only Claude profile that passes --strict-mcp-config and supplies
// a task-owned empty --mcp-config file. A future profile change must update
// this gate before ordinary user/project MCP can be treated as bypassed.
func validateMCPArguments(request task.TaskRecord) error {
	arguments, inputs, err := printArguments(request)
	if err != nil {
		return err
	}
	for _, flag := range []string{"--safe-mode", "--restricted", "--strict-mcp-config", "--no-chrome"} {
		if !hasClaudeFlag(arguments, flag) {
			return fmt.Errorf("%w: Claude policy inspection requires the strict MCP argv", ErrUnsupportedProfile)
		}
	}
	mcpArgumentIndex := -1
	for _, required := range []struct {
		flag  string
		value string
	}{
		{flag: "--tools", value: "Read,Glob,Grep"},
		{flag: "--disallowedTools", value: "mcp__*"},
		{flag: "--mcp-config", value: ""},
	} {
		index := claudeFlagValueIndex(arguments, required.flag, required.value)
		if index < 0 {
			return fmt.Errorf("%w: Claude policy inspection requires the strict MCP argv", ErrUnsupportedProfile)
		}
		if required.flag == "--mcp-config" {
			mcpArgumentIndex = index
		}
	}
	if !slices.ContainsFunc(inputs, func(input task.InputFile) bool {
		return input.Name == "empty-mcp.json" && input.ArgumentIndex == mcpArgumentIndex && input.Content == emptyMCPSettings
	}) {
		return fmt.Errorf("%w: Claude policy inspection requires a task-owned empty MCP input", ErrUnsupportedProfile)
	}
	return nil
}

func hasClaudeFlag(arguments []string, flag string) bool {
	for _, argument := range arguments {
		if argument == flag {
			return true
		}
	}
	return false
}

func claudeFlagValueIndex(arguments []string, flag, want string) int {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == flag && arguments[index+1] == want {
			return index + 1
		}
	}
	return -1
}

func currentClaudeOSUsername() (string, error) {
	identity, err := user.Current()
	if err != nil || identity.Username == "" || filepath.Base(identity.Username) != identity.Username || strings.ContainsRune(identity.Username, '\x00') {
		return "", fmt.Errorf("%w: native managed-policy username is unavailable", ErrUnsupportedProfile)
	}
	return identity.Username, nil
}

func claudeWorkspaceAncestors(workspace string) ([]string, error) {
	if workspace == "" || !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace || strings.ContainsRune(workspace, '\x00') {
		return nil, fmt.Errorf("%w: workspace must be a clean absolute path", ErrUnsupportedProfile)
	}
	ancestors := make([]string, 0, 8)
	for directory := workspace; ; directory = filepath.Dir(directory) {
		ancestors = append(ancestors, directory)
		if len(ancestors) > task.MaxPolicySources {
			return nil, fmt.Errorf("%w: workspace policy inventory exceeds the source bound", ErrUnsupportedProfile)
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
	}
	return ancestors, nil
}

func readClaudeSource(reader claudePolicySourceReader, path string) (commonprovider.SourceBytes, error) {
	source, err := reader(path)
	if err != nil {
		return source, fmt.Errorf("%w: inspect Claude policy source %s: %w", ErrUnsupportedProfile, path, err)
	}
	if source.Path != "" && source.Path != path {
		return source, fmt.Errorf("%w: policy source reader returned %s for %s", ErrUnsupportedProfile, source.Path, path)
	}
	if !source.Present {
		return commonprovider.SourceBytes{Path: path}, nil
	}
	if len(source.Data) > task.MaxControlRecordSize {
		return source, fmt.Errorf("%w: Claude policy source %s exceeds the size bound", ErrUnsupportedProfile, path)
	}
	return source, nil
}

func claudeAbsentSource(path, kind string) task.PolicySourceDigest {
	return task.PolicySourceDigest{Path: path, Kind: kind}
}

const claudePlaintextCredentialsFallbackKind = "claude-plaintext-credentials-fallback"

// observeClaudePlaintextCredentialsFallback deliberately uses Lstat only.
// Claude's native path checks Keychain first and falls back to
// ~/.claude/.credentials.json; the fallback is an OAuth credential store, so
// its contents must never be read, parsed, or hashed by policy inspection.
// An absent path is useful evidence. A present path is recorded as an opaque
// marker without opening it; the native helper remains the authority for the
// account actually used by the CLI. This allows a normal signed-in account to
// retain Claude's native fallback file without copying or exposing it.
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

// These controls are all source-backed by Claude's settings schema. They are
// either executable, alternate-auth, MCP, or permission/model controls that
// cannot be admitted without native schema evaluation. Restricted mode ignores
// ordinary settings, but rejecting them here keeps a changed flag interpretation
// from turning an observed source into an unreviewed capability.
var claudeSettingsControlKeys = map[string]struct{}{
	"adddir": {}, "additionaldirectories": {}, "agent": {}, "agents": {},
	"allowedmcpservers": {}, "allowedtools": {}, "apikeyhelper": {},
	"availablemodels": {}, "deniedmcpservers": {}, "disableallhooks": {},
	"disablecommandpluginsources": {}, "disablemcp": {}, "disallowedtools": {},
	"disabledmcpjsonservers": {}, "enabledmcpjsonservers": {},
	"enableallprojectmcpservers": {}, "enabledplugins": {},
	"extraknownmarketplaces": {}, "env": {}, "fallbackmodel": {},
	"filesuggestion": {}, "hooks": {}, "lspservers": {}, "managedmcpservers": {},
	"mcpservers": {}, "model": {}, "permissions": {}, "permissionmode": {},
	"pluginconfigs": {}, "plugindir": {}, "plugindirnomcp": {}, "plugins": {},
	"sandbox": {}, "statusline": {}, "settingsources": {},
	"awsauthrefresh": {}, "awscredentialexport": {}, "gcpauthrefresh": {},
	"otelheadershelper": {}, "policyhelpers": {}, "proxyauthhelper": {},
}

func rejectClaudeSettingsControls(value any, rejectUnknownTopLevel bool) error {
	return walkClaudeJSONKeys(value, func(key string, path []string) error {
		if _, found := claudeSettingsControlKeys[strings.ToLower(key)]; found {
			return fmt.Errorf("settings control %s is unsupported", strings.Join(append(path, key), "."))
		}
		if rejectUnknownTopLevel && len(path) == 0 && !claudeBenignSettingsKey(strings.ToLower(key)) {
			return fmt.Errorf("unrecognized settings field %q", key)
		}
		return nil
	})
}

func claudeBenignSettingsKey(key string) bool {
	// These keys are appearance, display, or local UX state. They cannot add a
	// tool, executable, auth source, writable root, model, or permission rule.
	_, ok := claudeBenignSettingsKeys[key]
	return ok
}

var claudeBenignSettingsKeys = map[string]struct{}{
	"attribution": {}, "autocompactenabled": {}, "autoscrollenabled": {},
	"autoupdates": {}, "difftool": {}, "editormode": {},
	"filecheckpointingenabled": {}, "includecoauthoredby": {},
	"preferrednotifchannel": {}, "showmessagetimestamps": {},
	"showturnduration": {}, "terminalprogressbarenabled": {}, "theme": {},
	"verbose": {},
}

func walkClaudeJSONKeys(value any, visit func(string, []string) error) error {
	return walkClaudeJSONValue(value, nil, visit)
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

func walkClaudeJSONValue(value any, path []string, visit func(string, []string) error) error {
	switch typed := value.(type) {
	case map[string]any:
		return walkClaudeJSONObject(typed, path, visit)
	case []any:
		return walkClaudeJSONArray(typed, path, visit)
	default:
		return nil
	}
}

func walkClaudeJSONObject(object map[string]any, path []string, visit func(string, []string) error) error {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if err := visit(key, path); err != nil {
			return err
		}
		if err := walkClaudeJSONValue(object[key], append(path, key), visit); err != nil {
			return err
		}
	}
	return nil
}

func walkClaudeJSONArray(array []any, path []string, visit func(string, []string) error) error {
	for index, item := range array {
		if err := walkClaudeJSONValue(item, append(path, fmt.Sprintf("[%d]", index)), visit); err != nil {
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
