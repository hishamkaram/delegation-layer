package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	readOnlyAgentName       = "delegation-layer-read-only"
	workspaceWriteAgentName = "delegation-layer-workspace-write"
)

// readOnlyConfigContent is an inline native OpenCode permission configuration.
// The catch-all deny keeps newly added tools disabled until the adapter has an
// explicit read-only decision for them. The effective configuration is also
// inspected immediately before admission and launch, because managed and
// legacy configuration sources can have higher precedence than inline config.
const readOnlyConfigContent = `{"agent":{"delegation-layer-read-only":{"permission":{"*":"deny","read":"allow","grep":"allow","glob":"allow","list":"allow","edit":"deny","bash":"deny","task":"deny","skill":"deny","lsp":"deny","question":"deny","webfetch":"deny","websearch":"deny","external_directory":"deny","doom_loop":"deny"}}},"permission":{"*":"deny","read":"allow","grep":"allow","glob":"allow","list":"allow","edit":"deny","bash":"deny","task":"deny","skill":"deny","lsp":"deny","question":"deny","webfetch":"deny","websearch":"deny","external_directory":"deny","doom_loop":"deny"},"lsp":false,"formatter":false,"compaction":{"auto":false}}`

const (
	readOnlyPolicyFacts       = "read_only"
	workspaceWritePolicyFacts = "workspace_write"
)

type policyFacts struct {
	ReadOnly       bool   `json:"read_only,omitempty"`
	WorkspaceWrite bool   `json:"workspace_write,omitempty"`
	ConfigSHA256   string `json:"config_sha256"`
}

type permissionMap map[string]json.RawMessage

func permissionValue(value string) json.RawMessage {
	// All callers use one of the two fixed OpenCode permission literals. Keeping
	// this construction allocation-free also avoids turning an impossible
	// marshal failure into an ignored error in the policy projector.
	return json.RawMessage(`"` + value + `"`)
}

var readOnlyPermission = permissionMap{
	"*":                  permissionValue("deny"),
	"read":               permissionValue("allow"),
	"grep":               permissionValue("allow"),
	"glob":               permissionValue("allow"),
	"list":               permissionValue("allow"),
	"edit":               permissionValue("deny"),
	"bash":               permissionValue("deny"),
	"task":               permissionValue("deny"),
	"skill":              permissionValue("deny"),
	"lsp":                permissionValue("deny"),
	"question":           permissionValue("deny"),
	"webfetch":           permissionValue("deny"),
	"websearch":          permissionValue("deny"),
	"external_directory": permissionValue("deny"),
	"doom_loop":          permissionValue("deny"),
}

func workspaceWritePermission(workspace string) (permissionMap, error) {
	if !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace {
		return nil, fmt.Errorf("invalid workspace path for OpenCode permission policy")
	}
	if err := validateWorkspaceTree(workspace); err != nil {
		return nil, err
	}
	worktree, err := openCodeWorktreeRoot(workspace)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(worktree, workspace)
	if err != nil || relative == ".." || len(relative) >= 3 && relative[:3] == ".."+string(filepath.Separator) {
		return nil, fmt.Errorf("invalid workspace relative to OpenCode worktree")
	}
	// OpenCode's native apply_patch checks an allowed source path, but its
	// destination check treats every path in the enclosing Git worktree as an
	// internal path. A nested workspace would therefore allow a move from the
	// selected directory into a sibling (including the delegation state root).
	// Keep workspace-write available for a whole checkout, where both endpoints
	// are inside the caller-selected boundary, and fail closed for nested trees.
	if relative != "." {
		return nil, fmt.Errorf("OpenCode workspace-write requires the Git worktree root to contain patch moves")
	}
	// OpenCode treats `*` and `?` as glob operators in permission keys. A
	// literal workspace name containing either character could therefore widen
	// the allow rule to a sibling directory. Reject glob metacharacters in the
	// relative prefix rather than attempting to escape a provider-owned matcher.
	if strings.ContainsAny(relative, "*?[]{}") {
		return nil, fmt.Errorf("workspace path contains OpenCode permission glob metacharacters")
	}
	editPattern := "**"
	if relative != "." {
		editPattern = filepath.ToSlash(filepath.Join(relative, "**"))
	}
	editRules, err := json.Marshal(map[string]string{"*": "deny", editPattern: "allow"})
	if err != nil {
		return nil, fmt.Errorf("marshal OpenCode edit policy: %w", err)
	}
	return permissionMap{
		"*":                  permissionValue("deny"),
		"read":               permissionValue("allow"),
		"grep":               permissionValue("allow"),
		"glob":               permissionValue("allow"),
		"list":               permissionValue("allow"),
		"edit":               json.RawMessage(editRules),
		"bash":               permissionValue("deny"),
		"task":               permissionValue("deny"),
		"skill":              permissionValue("deny"),
		"lsp":                permissionValue("deny"),
		"question":           permissionValue("deny"),
		"webfetch":           permissionValue("deny"),
		"websearch":          permissionValue("deny"),
		"external_directory": permissionValue("deny"),
		"doom_loop":          permissionValue("deny"),
	}, nil
}

// validateWorkspaceTree makes the native path rule fail closed for symlinked
// workspaces. OpenCode's edit permission matcher is lexical and its write
// tool follows an existing symlink, so allowing one inside the selected tree
// would let an otherwise permitted relative path reach another directory.
// Walk uses Lstat semantics and does not follow symlink directories. The
// check is repeated during the fresh preflight immediately before launch.
func validateWorkspaceTree(workspace string) error {
	info, err := os.Lstat(workspace)
	if err != nil {
		return fmt.Errorf("inspect OpenCode workspace: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("OpenCode workspace is not a directory")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("OpenCode workspace cannot be a symlink")
	}
	if err := filepath.Walk(workspace, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("OpenCode workspace contains symlink %q", path)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("validate OpenCode workspace containment: %w", err)
	}
	return nil
}

// openCodeWorktreeRoot mirrors the path namespace OpenCode uses when it asks
// for edit permission: paths are relative to the enclosing Git worktree. A
// workspace without a Git marker is its own worktree for this purpose.
func openCodeWorktreeRoot(workspace string) (string, error) {
	marker := ""
	for directory := workspace; ; directory = filepath.Dir(directory) {
		candidate := filepath.Join(directory, ".git")
		info, err := os.Lstat(candidate)
		if err == nil {
			if info.Mode().IsRegular() || info.IsDir() {
				marker = candidate
				break
			}
			return "", fmt.Errorf("unsupported Git marker for OpenCode worktree")
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("inspect OpenCode worktree marker: %w", err)
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
	}
	if marker == "" {
		return workspace, nil
	}
	// OpenCode delegates its checkout boundary to Git. A repository can set
	// core.worktree to a directory different from the directory containing its
	// .git marker, so resolve that setting from the same repository metadata
	// instead of assuming the marker's parent is the writable worktree. This is
	// deliberately a bounded file read rather than a child process: admission
	// remains nonblocking, and no ambient GIT_DIR/GIT_WORK_TREE override can
	// change the result seen by the provider's sanitized launch environment.
	gitDir, err := gitDirectory(marker)
	if err != nil {
		return "", err
	}
	worktree, configured, err := gitCoreWorktree(gitDir)
	if err != nil {
		return "", err
	}
	if !configured {
		worktree = filepath.Dir(marker)
	}
	canonical, err := config.CanonicalizePath(worktree)
	if err != nil || canonical != filepath.Clean(worktree) {
		return "", fmt.Errorf("git returned a noncanonical OpenCode worktree")
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("OpenCode Git worktree is not a directory")
	}
	return canonical, nil
}

func gitDirectory(marker string) (string, error) {
	info, err := os.Lstat(marker)
	if err != nil {
		return "", fmt.Errorf("inspect OpenCode Git marker: %w", err)
	}
	if info.IsDir() {
		return marker, nil
	}
	source, err := commonprovider.ReadPolicySource(marker)
	if err != nil {
		return "", fmt.Errorf("inspect OpenCode Git marker: %w", err)
	}
	if !source.Present {
		return "", fmt.Errorf("OpenCode Git marker disappeared")
	}
	line := strings.TrimSpace(string(source.Data))
	if !strings.HasPrefix(strings.ToLower(line), "gitdir:") {
		return "", fmt.Errorf("OpenCode Git marker has no gitdir")
	}
	value := strings.TrimSpace(line[len("gitdir:"):])
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("OpenCode Git marker has an invalid gitdir")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(filepath.Dir(marker), value)
	}
	canonical, err := config.CanonicalizePath(value)
	if err != nil {
		return "", fmt.Errorf("resolve OpenCode Git directory: %w", err)
	}
	return canonical, nil
}

func gitCoreWorktree(gitDir string) (string, bool, error) {
	configPaths, err := gitConfigPaths(gitDir)
	if err != nil {
		return "", false, err
	}
	resolution := gitWorktreeResolution{}
	for _, path := range configPaths {
		values, readErr := readGitConfig(path)
		if readErr != nil {
			return "", false, readErr
		}
		if applyErr := resolution.apply(path, values); applyErr != nil {
			return "", false, applyErr
		}
	}
	// Git reads config.worktree only when the common configuration enables the
	// extensions.worktreeConfig feature. Ignoring that file otherwise is
	// necessary to match Git's effective worktree boundary.
	if resolution.enabled {
		path := filepath.Join(gitDir, "config.worktree")
		values, readErr := readGitConfig(path)
		if readErr != nil {
			return "", false, readErr
		}
		if applyErr := resolution.apply(path, values); applyErr != nil {
			return "", false, applyErr
		}
	}
	return resolution.resolved, resolution.configured, nil
}

func gitConfigPaths(gitDir string) ([]string, error) {
	paths := []string{filepath.Join(gitDir, "config")}
	source, err := commonprovider.ReadPolicySource(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return nil, fmt.Errorf("inspect OpenCode Git common directory: %w", err)
	}
	if !source.Present {
		return paths, nil
	}
	value := strings.TrimSpace(string(source.Data))
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return nil, fmt.Errorf("OpenCode Git common directory is invalid")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(gitDir, value)
	}
	commonPath, err := config.CanonicalizePath(value)
	if err != nil {
		return nil, fmt.Errorf("resolve OpenCode Git common directory: %w", err)
	}
	return append(paths, filepath.Join(commonPath, "config")), nil
}

type gitConfigValues struct {
	CoreWorktree      string
	CoreWorktreeSet   bool
	WorktreeConfig    bool
	WorktreeConfigSet bool
}

type gitWorktreeResolution struct {
	resolved   string
	configured bool
	enabled    bool
}

func (resolution *gitWorktreeResolution) apply(path string, values gitConfigValues) error {
	if values.WorktreeConfigSet {
		resolution.enabled = values.WorktreeConfig
	}
	if !values.CoreWorktreeSet {
		return nil
	}
	if values.CoreWorktree == "" {
		return fmt.Errorf("OpenCode Git core.worktree is empty")
	}
	resolution.resolved = resolveGitConfigPath(path, values.CoreWorktree)
	resolution.configured = true
	return nil
}

func readGitConfig(path string) (gitConfigValues, error) {
	source, err := commonprovider.ReadPolicySource(path)
	if err != nil {
		return gitConfigValues{}, fmt.Errorf("inspect OpenCode Git config: %w", err)
	}
	if !source.Present {
		return gitConfigValues{}, nil
	}
	return parseGitConfig(source.Data)
}

func parseGitConfig(data []byte) (gitConfigValues, error) {
	var result gitConfigValues
	section := ""
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := stripGitConfigComment(strings.TrimSuffix(rawLine, "\r"))
		if line == "" {
			continue
		}
		updatedSection, isSection, sectionErr := parseGitSection(line)
		if sectionErr != nil {
			return result, sectionErr
		}
		if isSection {
			section = updatedSection
			continue
		}
		key, rawValue, settingErr := parseGitSetting(line)
		if settingErr != nil {
			return result, settingErr
		}
		if strings.HasPrefix(key, "include") {
			return result, fmt.Errorf("OpenCode Git config includes are unsupported")
		}
		if applyErr := applyGitSetting(&result, section, key, rawValue); applyErr != nil {
			return result, applyErr
		}
	}
	return result, nil
}

func parseGitSection(line string) (string, bool, error) {
	if !strings.HasPrefix(line, "[") {
		return "", false, nil
	}
	if !strings.HasSuffix(line, "]") {
		return "", true, fmt.Errorf("OpenCode Git config has an invalid section header")
	}
	section := strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
	if strings.HasPrefix(section, "include") {
		return "", true, fmt.Errorf("OpenCode Git config includes are unsupported")
	}
	return section, true, nil
}

func parseGitSetting(line string) (string, string, error) {
	key, value, ok := strings.Cut(line, "=")
	if !ok {
		fields := strings.Fields(line)
		if len(fields) == 1 {
			// Git treats a valueless setting as the boolean value true. Keep
			// that native interpretation so unrelated boolean settings do not
			// make an otherwise valid checkout fail admission.
			return strings.ToLower(strings.TrimSpace(fields[0])), "true", nil
		}
		if len(fields) != 2 {
			return "", "", fmt.Errorf("OpenCode Git config has an invalid setting")
		}
		key, value = fields[0], fields[1]
	}
	return strings.ToLower(strings.TrimSpace(key)), value, nil
}

func applyGitSetting(result *gitConfigValues, section, key, rawValue string) error {
	switch section {
	case "core":
		if key != "worktree" {
			return nil
		}
		value, err := parseGitConfigValue(rawValue)
		if err != nil {
			return err
		}
		result.CoreWorktree, result.CoreWorktreeSet = value, true
	case "extensions":
		if key != "worktreeconfig" {
			return nil
		}
		value, err := parseGitConfigBool(rawValue)
		if err != nil {
			return err
		}
		result.WorktreeConfig, result.WorktreeConfigSet = value, true
	}
	return nil
}

func resolveGitConfigPath(configPath, value string) string {
	if filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(filepath.Dir(configPath), value)
}

func parseGitConfigValue(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if strings.HasPrefix(value, `"`) {
		decoded, err := strconv.Unquote(value)
		if err != nil {
			return "", fmt.Errorf("OpenCode Git config has invalid quoting")
		}
		value = decoded
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("OpenCode Git config has invalid characters")
	}
	return value, nil
}

func parseGitConfigBool(raw string) (bool, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "true", "yes", "on", "1":
		return true, nil
	case "false", "no", "off", "0":
		return false, nil
	default:
		return false, fmt.Errorf("OpenCode Git extensions.worktreeConfig has an invalid value")
	}
}

func stripGitConfigComment(raw string) string {
	inQuote := false
	escaped := false
	for index, value := range raw {
		if escaped {
			escaped = false
			continue
		}
		if inQuote && value == '\\' {
			escaped = true
			continue
		}
		if value == '"' {
			inQuote = !inQuote
			continue
		}
		if !inQuote && (value == '#' || value == ';') {
			return strings.TrimSpace(raw[:index])
		}
	}
	return strings.TrimSpace(raw)
}

func workspaceWriteConfigContent(workspace string) (string, error) {
	permissions, err := workspaceWritePermission(workspace)
	if err != nil {
		return "", err
	}
	type agentConfig struct {
		Permission permissionMap `json:"permission"`
	}
	content := struct {
		Agent      map[string]agentConfig `json:"agent"`
		Permission permissionMap          `json:"permission"`
		LSP        bool                   `json:"lsp"`
		Formatter  bool                   `json:"formatter"`
		Compaction struct {
			Auto bool `json:"auto"`
		} `json:"compaction"`
	}{
		Agent:      map[string]agentConfig{workspaceWriteAgentName: {Permission: permissions}},
		Permission: permissions,
		LSP:        false,
		Formatter:  false,
		Compaction: struct {
			Auto bool `json:"auto"`
		}{Auto: false},
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return "", fmt.Errorf("marshal OpenCode workspace policy: %w", err)
	}
	return string(encoded), nil
}

type effectiveConfig struct {
	Agent      map[string]effectiveAgent  `json:"agent"`
	Mode       map[string]json.RawMessage `json:"mode"`
	Permission json.RawMessage            `json:"permission"`
	MCP        map[string]json.RawMessage `json:"mcp"`
	LSP        json.RawMessage            `json:"lsp"`
	Formatter  json.RawMessage            `json:"formatter"`
	Compaction json.RawMessage            `json:"compaction"`
}

type effectiveAgent struct {
	Disable    *bool           `json:"disable"`
	Mode       string          `json:"mode"`
	Permission json.RawMessage `json:"permission"`
}

// projectReadOnlyConfig accepts the debug config JSON only when every
// permission decision relevant to this adapter is still the compiled
// read-only policy. It returns a fixed nonsecret fact instead of persisting
// the user's complete OpenCode configuration.
func projectReadOnlyConfig(native []byte) (json.RawMessage, error) {
	return projectPolicyConfig(native, readOnlyAgentName, readOnlyPermission, readOnlyPolicyFacts, "read-only")
}

// projectWorkspaceWriteConfig accepts only the exact native permission
// binding emitted by workspaceWriteConfigContent. The explicit
// external_directory denial is the native containment boundary for writes.
func projectWorkspaceWriteConfig(native []byte, workspace string) (json.RawMessage, error) {
	permissions, err := workspaceWritePermission(workspace)
	if err != nil {
		return nil, err
	}
	return projectPolicyConfig(native, workspaceWriteAgentName, permissions, workspaceWritePolicyFacts, "workspace-write")
}

func projectPolicyConfig(native []byte, agentName string, want permissionMap, facts string, mode string) (json.RawMessage, error) {
	if err := task.ValidateJSONStructure(native); err != nil {
		return nil, err
	}
	var config effectiveConfig
	if err := json.Unmarshal(native, &config); err != nil {
		return nil, err
	}
	if err := validateProjectedPolicy(config, agentName, want, mode); err != nil {
		return nil, err
	}
	configSHA256, digestErr := openCodeConfigDigest(native)
	if digestErr != nil {
		return nil, digestErr
	}
	projected, factsErr := policyFactsForMode(facts, configSHA256, mode)
	if factsErr != nil {
		return nil, factsErr
	}
	encoded, err := task.MarshalCanonical(projected)
	if err != nil {
		return nil, fmt.Errorf("marshal OpenCode %s policy facts: %w", mode, err)
	}
	return encoded, nil
}

func validateProjectedPolicy(config effectiveConfig, agentName string, want permissionMap, mode string) error {
	if !samePermissionJSON(config.Permission, want, mode) {
		return fmt.Errorf("effective OpenCode global permissions differ from %s policy", mode)
	}
	agent, ok := config.Agent[agentName]
	if !ok || !samePermissionJSON(agent.Permission, want, mode) {
		return fmt.Errorf("effective OpenCode agent permissions differ from %s policy", mode)
	}
	if agent.Disable != nil && *agent.Disable {
		return fmt.Errorf("effective OpenCode %s agent is disabled", mode)
	}
	if agent.Mode != "" && agent.Mode != "all" && agent.Mode != "primary" {
		return fmt.Errorf("effective OpenCode %s agent mode is not primary", mode)
	}
	if _, overridden := config.Mode[agentName]; overridden {
		return fmt.Errorf("effective OpenCode legacy mode overrides %s policy", mode)
	}
	if err := rejectEnabledMCP(config.MCP); err != nil {
		return err
	}
	if !jsonFalse(config.LSP) || !jsonFalse(config.Formatter) || !jsonObjectBoolFalse(config.Compaction, "auto") {
		return fmt.Errorf("effective OpenCode lsp, formatter, or compaction configuration differs from %s policy", mode)
	}
	return nil
}

func policyFactsForMode(facts, configSHA256, mode string) (policyFacts, error) {
	projected := policyFacts{ConfigSHA256: configSHA256}
	switch facts {
	case readOnlyPolicyFacts:
		projected.ReadOnly = true
	case workspaceWritePolicyFacts:
		projected.WorkspaceWrite = true
	default:
		return policyFacts{}, fmt.Errorf("invalid OpenCode %s policy fact kind", mode)
	}
	return projected, nil
}

// rejectEnabledMCP closes a command-execution path that OpenCode evaluates
// before tool permissions. `--pure` disables external plugins but does not
// disable configured local or remote MCP servers; an enabled local server can
// therefore spawn its own command outside the adapter's tool boundary.
func rejectEnabledMCP(servers map[string]json.RawMessage) error {
	for name, raw := range servers {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil || object == nil {
			return fmt.Errorf("effective OpenCode MCP server %q has an invalid configuration", name)
		}
		enabledRaw, exists := object["enabled"]
		if exists {
			var enabled bool
			if err := json.Unmarshal(enabledRaw, &enabled); err != nil {
				return fmt.Errorf("effective OpenCode MCP server %q has an invalid enabled setting", name)
			}
			if !enabled {
				continue
			}
		}
		return fmt.Errorf("effective OpenCode MCP server %q is enabled", name)
	}
	return nil
}

func samePermissions(got, want permissionMap) bool {
	if len(got) != len(want) {
		return false
	}
	for key, value := range want {
		gotValue, err := canonicalJSON(got[key])
		wantValue, wantErr := canonicalJSON(value)
		if err != nil || wantErr != nil || !bytes.Equal(gotValue, wantValue) {
			return false
		}
	}
	return true
}

func samePermissionJSON(got json.RawMessage, want permissionMap, mode string) bool {
	value, err := parseOrderedJSON(got)
	if err != nil || value.kind != orderedJSONObject {
		return false
	}
	keys := expectedPermissionOrder(mode, want)
	if len(value.fields) != len(keys) || len(keys) != len(want) {
		return false
	}
	for index, field := range value.fields {
		if field.key != keys[index] {
			return false
		}
		actual, actualErr := field.value.encode()
		expected, expectedErr := canonicalJSON(want[field.key])
		if actualErr != nil || expectedErr != nil || !bytes.Equal(actual, expected) {
			return false
		}
	}
	return true
}

func expectedPermissionOrder(mode string, want permissionMap) []string {
	if mode == ModeReadOnly {
		return []string{
			"*", "read", "grep", "glob", "list", "edit", "bash", "task",
			"skill", "lsp", "question", "webfetch", "websearch", "external_directory", "doom_loop",
		}
	}
	keys := make([]string, 0, len(want))
	for key := range want {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	value, err := parseOrderedJSON(raw)
	if err != nil {
		return nil, err
	}
	return value.encode()
}

const (
	orderedJSONScalar byte = iota
	orderedJSONObject
	orderedJSONArray
)

type orderedJSON struct {
	kind   byte
	scalar json.RawMessage
	fields []orderedJSONField
	items  []orderedJSON
}

type orderedJSONField struct {
	key   string
	value orderedJSON
}

func parseOrderedJSON(raw []byte) (orderedJSON, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeOrderedJSON(decoder)
	if err != nil {
		return orderedJSON{}, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return orderedJSON{}, fmt.Errorf("trailing JSON value")
		}
		return orderedJSON{}, err
	}
	return value, nil
}

func decodeOrderedJSON(decoder *json.Decoder) (orderedJSON, error) {
	token, err := decoder.Token()
	if err != nil {
		return orderedJSON{}, err
	}
	switch typed := token.(type) {
	case json.Delim:
		switch typed {
		case '{':
			return decodeOrderedJSONObject(decoder)
		case '[':
			return decodeOrderedJSONArray(decoder)
		default:
			return orderedJSON{}, fmt.Errorf("unexpected JSON delimiter %q", typed)
		}
	case string, bool, json.Number:
		encoded, marshalErr := json.Marshal(typed)
		if marshalErr != nil {
			return orderedJSON{}, marshalErr
		}
		return orderedJSON{kind: orderedJSONScalar, scalar: encoded}, nil
	case nil:
		return orderedJSON{kind: orderedJSONScalar, scalar: []byte("null")}, nil
	default:
		return orderedJSON{}, fmt.Errorf("unsupported JSON token %T", token)
	}
}

func decodeOrderedJSONObject(decoder *json.Decoder) (orderedJSON, error) {
	fields := make([]orderedJSONField, 0)
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return orderedJSON{}, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return orderedJSON{}, fmt.Errorf("JSON object key is not a string")
		}
		if _, duplicate := seen[key]; duplicate {
			return orderedJSON{}, fmt.Errorf("duplicate JSON object key %q", key)
		}
		seen[key] = struct{}{}
		value, err := decodeOrderedJSON(decoder)
		if err != nil {
			return orderedJSON{}, err
		}
		fields = append(fields, orderedJSONField{key: key, value: value})
	}
	close, err := decoder.Token()
	if err != nil {
		return orderedJSON{}, err
	}
	if close != json.Delim('}') {
		return orderedJSON{}, fmt.Errorf("JSON object was not closed")
	}
	return orderedJSON{kind: orderedJSONObject, fields: fields}, nil
}

func decodeOrderedJSONArray(decoder *json.Decoder) (orderedJSON, error) {
	items := make([]orderedJSON, 0)
	for decoder.More() {
		value, err := decodeOrderedJSON(decoder)
		if err != nil {
			return orderedJSON{}, err
		}
		items = append(items, value)
	}
	close, err := decoder.Token()
	if err != nil {
		return orderedJSON{}, err
	}
	if close != json.Delim(']') {
		return orderedJSON{}, fmt.Errorf("JSON array was not closed")
	}
	return orderedJSON{kind: orderedJSONArray, items: items}, nil
}

func (value orderedJSON) encode() ([]byte, error) {
	switch value.kind {
	case orderedJSONScalar:
		return slices.Clone(value.scalar), nil
	case orderedJSONArray:
		encoded := []byte{'['}
		for index, item := range value.items {
			if index > 0 {
				encoded = append(encoded, ',')
			}
			itemEncoded, err := item.encode()
			if err != nil {
				return nil, err
			}
			encoded = append(encoded, itemEncoded...)
		}
		return append(encoded, ']'), nil
	case orderedJSONObject:
		encoded := []byte{'{'}
		for index, field := range value.fields {
			if index > 0 {
				encoded = append(encoded, ',')
			}
			key, err := json.Marshal(field.key)
			if err != nil {
				return nil, err
			}
			encoded = append(encoded, key...)
			encoded = append(encoded, ':')
			valueEncoded, err := field.value.encode()
			if err != nil {
				return nil, err
			}
			encoded = append(encoded, valueEncoded...)
		}
		return append(encoded, '}'), nil
	default:
		return nil, fmt.Errorf("unknown ordered JSON value kind %d", value.kind)
	}
}

func jsonFalse(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("false"))
}

func jsonObjectBoolFalse(raw json.RawMessage, key string) bool {
	if len(raw) == 0 {
		return false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return false
	}
	return jsonFalse(object[key])
}

func validateReadOnlyPolicyFacts(native json.RawMessage) (string, error) {
	return validatePolicyFacts(native, readOnlyPolicyFacts, "read-only")
}

func validateWorkspaceWritePolicyFacts(native json.RawMessage) (string, error) {
	return validatePolicyFacts(native, workspaceWritePolicyFacts, "workspace-write")
}

func validatePolicyFacts(native json.RawMessage, expected, mode string) (string, error) {
	if err := task.ValidateJSONStructure(native); err != nil {
		return "", fmt.Errorf("invalid OpenCode %s policy facts: %w", mode, err)
	}
	var facts policyFacts
	if err := task.DecodeStrict(native, &facts); err != nil {
		return "", fmt.Errorf("invalid OpenCode %s policy facts: %w", mode, err)
	}
	if err := task.ValidateSHA256(facts.ConfigSHA256); err != nil {
		return "", fmt.Errorf("invalid OpenCode %s policy config digest: %w", mode, err)
	}
	switch expected {
	case readOnlyPolicyFacts:
		if !facts.ReadOnly || facts.WorkspaceWrite {
			return "", fmt.Errorf("invalid OpenCode %s policy facts", mode)
		}
	case workspaceWritePolicyFacts:
		if !facts.WorkspaceWrite || facts.ReadOnly {
			return "", fmt.Errorf("invalid OpenCode %s policy facts", mode)
		}
	default:
		return "", fmt.Errorf("invalid OpenCode %s policy fact kind", mode)
	}
	return facts.ConfigSHA256, nil
}

// openCodeConfigDigest binds the effective native policy without persisting
// provider credentials. Sensitive values are replaced by type markers while
// keys, shapes, nonsecret settings, and path choices remain part of the
// digest. This lets a fresh debug-config projection detect policy drift while
// keeping the inspection receipt nonsecret.
func openCodeConfigDigest(native []byte) (string, error) {
	if err := task.ValidateJSONStructure(native); err != nil {
		return "", err
	}
	value, err := parseOrderedJSON(native)
	if err != nil {
		return "", fmt.Errorf("decode OpenCode effective configuration: %w", err)
	}
	projected := projectOpenCodeJSON(value, "", false)
	encoded, err := projected.encode()
	if err != nil {
		return "", fmt.Errorf("marshal OpenCode effective configuration projection: %w", err)
	}
	return task.ComputeSHA256(encoded), nil
}

// projectOpenCodeJSON redacts sensitive values while retaining the ordering of
// permission-rule objects. OpenCode evaluates overlapping rules in declaration
// order (the last matching rule wins), so sorting those objects would turn a
// policy change into the same digest and could admit a widened or narrowed
// workspace boundary. Ordinary configuration objects are sorted for stable
// digests when their JSON key order is semantically irrelevant.
func projectOpenCodeJSON(value orderedJSON, key string, permissionRules bool) orderedJSON {
	if sensitiveOpenCodeKey(key) {
		return orderedJSON{
			kind: orderedJSONObject,
			fields: []orderedJSONField{{key: "redacted", value: orderedJSON{
				kind:   orderedJSONScalar,
				scalar: []byte(strconv.Quote(orderedJSONValueType(value))),
			}}},
		}
	}
	switch value.kind {
	case orderedJSONObject:
		projected := make([]orderedJSONField, 0, len(value.fields))
		for _, field := range value.fields {
			projected = append(projected, orderedJSONField{
				key:   field.key,
				value: projectOpenCodeJSON(field.value, field.key, key == "permission"),
			})
		}
		if !permissionRules {
			sort.Slice(projected, func(left, right int) bool {
				return projected[left].key < projected[right].key
			})
		}
		return orderedJSON{kind: orderedJSONObject, fields: projected}
	case orderedJSONArray:
		projected := make([]orderedJSON, len(value.items))
		for index, childValue := range value.items {
			projected[index] = projectOpenCodeJSON(childValue, "", false)
		}
		return orderedJSON{kind: orderedJSONArray, items: projected}
	default:
		return value
	}
}

func sensitiveOpenCodeKey(key string) bool {
	switch strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", ""), "_", "")) {
	case "key", "apikey", "token", "secret", "password", "credential", "authorization", "cookie", "headers", "env":
		return true
	default:
		return false
	}
}

func orderedJSONValueType(value orderedJSON) string {
	switch value.kind {
	case orderedJSONScalar:
		if bytes.Equal(value.scalar, []byte("null")) {
			return "null"
		}
		if len(value.scalar) > 0 && value.scalar[0] == '"' {
			return "string"
		}
		if bytes.Equal(value.scalar, []byte("true")) || bytes.Equal(value.scalar, []byte("false")) {
			return "bool"
		}
		return "number"
	case orderedJSONArray:
		return "array"
	case orderedJSONObject:
		return "object"
	default:
		return "unknown"
	}
}

func environmentForMode(values []string, mode, workspace string) ([]string, error) {
	result := slices.Clone(values)
	switch mode {
	case ModeReadOnly:
		// Project config can contain legacy mode entries that are merged after
		// inline config. Disable that source and verify the resulting effective
		// policy with `debug config` before the provider is admitted or launched.
		var err error
		result, err = withDelegationConfigHome(result)
		if err != nil {
			return nil, err
		}
		result = append(result,
			"OPENCODE_CONFIG_CONTENT="+readOnlyConfigContent,
			"OPENCODE_DISABLE_PROJECT_CONFIG=1",
		)
	case ModeWorkspaceWrite:
		// Workspace-write uses path-specific native edit rules in addition to
		// external-directory denial. Disable project config so a managed or
		// repository policy cannot silently widen this boundary. Disable global
		// and system Git configuration as well: OpenCode asks Git for its
		// enclosing worktree, so an ambient core.worktree override must not
		// disagree with the boundary calculated above.
		content, err := workspaceWriteConfigContent(workspace)
		if err != nil {
			return nil, err
		}
		result, err = withDelegationConfigHome(result)
		if err != nil {
			return nil, err
		}
		result = append(result,
			"OPENCODE_CONFIG_CONTENT="+content,
			"OPENCODE_DISABLE_PROJECT_CONFIG=1",
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL=/dev/null",
		)
	}
	slices.Sort(result)
	return result, nil
}

func withDelegationConfigHome(values []string) ([]string, error) {
	home := environmentValue(values, "HOME")
	if home == "" {
		return nil, fmt.Errorf("%w: invalid HOME", ErrUnsupportedProfile)
	}
	configHome, err := delegationConfigHome(home, environmentValue(values, "XDG_CONFIG_HOME"))
	if err != nil {
		return nil, err
	}
	return replaceEnvironmentValue(values, "XDG_CONFIG_HOME", configHome), nil
}

func environmentValue(values []string, key string) string {
	prefix := key + "="
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return strings.TrimPrefix(value, prefix)
		}
	}
	return ""
}

func replaceEnvironmentValue(values []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(values)+1)
	for _, entry := range values {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		result = append(result, entry)
	}
	return append(result, prefix+value)
}
