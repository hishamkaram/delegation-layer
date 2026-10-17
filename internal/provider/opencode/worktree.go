package opencode

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

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
