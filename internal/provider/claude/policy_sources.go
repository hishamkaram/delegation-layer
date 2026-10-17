package claude

import (
	"cmp"
	"fmt"
	"os/user"
	"path/filepath"
	"slices"
	"strings"

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
// used by the native CLI. A present managed plist/managed JSON source is
// refused because this pure resolver cannot faithfully parse the native
// whole-document policy semantics. Authentication remains the provider CLI's
// live responsibility; the legacy native inspection path is retained only for
// historical task reconstruction.
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

// validateMCPArguments binds the source projection below to the exact
// mode-specific Claude profile. Both modes pass --strict-mcp-config and
// supply a task-owned empty --mcp-config file; the write profile additionally
// enables the native file-edit tools. A future profile change must update this
// gate before ordinary user/project MCP can be treated as bypassed.
func validateMCPArguments(request task.TaskRecord) error {
	arguments, inputs, err := legacyPrintArguments(request)
	if err != nil {
		return err
	}
	for _, flag := range []string{"--safe-mode", "--restricted", "--strict-mcp-config", "--no-chrome"} {
		if !hasClaudeFlag(arguments, flag) {
			return fmt.Errorf("%w: Claude policy inspection requires the strict MCP argv", ErrUnsupportedProfile)
		}
	}
	mcpArgumentIndex := -1
	tools := "Read,Glob,Grep"
	if request.Mode == WorkspaceWriteMode {
		tools = "Read,Edit,Write,Glob,Grep"
	}
	for _, required := range []struct {
		flag  string
		value string
	}{
		{flag: "--tools", value: tools},
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
