package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
)

type inlinePermissionConfig struct {
	Agent map[string]struct {
		Permission permissionMap `json:"permission"`
	} `json:"agent"`
	Permission permissionMap   `json:"permission"`
	LSP        json.RawMessage `json:"lsp"`
	Formatter  json.RawMessage `json:"formatter"`
}

func TestReadOnlyEnvironmentInstallsNativePermissionOverrides(t *testing.T) {
	home := canonicalTestPath(t, "/tmp/home")
	values, err := environmentForMode([]string{"PATH=/bin", "HOME=" + home}, ModeReadOnly, canonicalTestPath(t, "/workspace"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.IsSorted(values) {
		t.Fatalf("environment is not canonical: %q", values)
	}
	if !slices.Contains(values, "OPENCODE_DISABLE_PROJECT_CONFIG=1") {
		t.Fatal("read-only environment did not disable project configuration")
	}
	if !slices.Contains(values, "XDG_CONFIG_HOME="+filepath.Join(home, ".config/delegation-layer-opencode")) {
		t.Fatalf("read-only environment did not isolate OpenCode config: %q", values)
	}
	config := inlineConfig(t, values)
	if !jsonFalse(config.LSP) || !jsonFalse(config.Formatter) {
		t.Fatal("read-only config unexpectedly enabled native services")
	}
	assertPermissionStrings(t, config.Permission, []string{"*", "edit", "bash", "task", "external_directory"}, "deny")
	assertPermissionStrings(t, config.Permission, []string{"read", "grep", "glob", "list"}, "allow")
	agent, ok := config.Agent[readOnlyAgentName]
	if !ok {
		t.Fatalf("read-only agent config is missing: %+v", config.Agent)
	}
	assertPermissionStrings(t, agent.Permission, []string{"*", "edit", "bash", "task", "external_directory"}, "deny")
}

func TestReadOnlyPolicyProjectionAcceptsCompiledEffectiveConfig(t *testing.T) {
	facts, err := projectReadOnlyConfig([]byte(readOnlyConfigContent))
	if err != nil {
		t.Fatalf("facts=%q err=%v", facts, err)
	}
	if _, err := validateReadOnlyPolicyFacts(facts); err != nil {
		t.Fatalf("projected facts failed validation: %v", err)
	}
}

func TestReadOnlyPolicyProjectionRejectsOverrides(t *testing.T) {
	for _, testCase := range []struct {
		name string
		json string
	}{
		{
			name: "agent edit allow",
			json: `{"agent":{"delegation-layer-read-only":{"permission":{"*":"deny","read":"allow","grep":"allow","glob":"allow","list":"allow","edit":"allow","bash":"deny","task":"deny","skill":"deny","lsp":"deny","question":"deny","webfetch":"deny","websearch":"deny","external_directory":"deny","doom_loop":"deny"}}},"permission":{"*":"deny","read":"allow","grep":"allow","glob":"allow","list":"allow","edit":"deny","bash":"deny","task":"deny","skill":"deny","lsp":"deny","question":"deny","webfetch":"deny","websearch":"deny","external_directory":"deny","doom_loop":"deny"}}`,
		},
		{
			name: "legacy mode override",
			json: `{"agent":{"delegation-layer-read-only":{"permission":{"*":"deny","read":"allow","grep":"allow","glob":"allow","list":"allow","edit":"deny","bash":"deny","task":"deny","skill":"deny","lsp":"deny","question":"deny","webfetch":"deny","websearch":"deny","external_directory":"deny","doom_loop":"deny"}}},"mode":{"delegation-layer-read-only":{"permission":{"edit":"allow"}}},"permission":{"*":"deny","read":"allow","grep":"allow","glob":"allow","list":"allow","edit":"deny","bash":"deny","task":"deny","skill":"deny","lsp":"deny","question":"deny","webfetch":"deny","websearch":"deny","external_directory":"deny","doom_loop":"deny"}}`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := projectReadOnlyConfig([]byte(testCase.json)); err == nil {
				t.Fatal("effective permission override was accepted")
			}
		})
	}
}

func TestPolicyProjectionRejectsAgentFallbackModes(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		before string
		after  string
	}{
		{
			name:   "disabled agent",
			before: `"delegation-layer-read-only":{"permission":`,
			after:  `"delegation-layer-read-only":{"disable":true,"permission":`,
		},
		{
			name:   "subagent mode",
			before: `"delegation-layer-read-only":{"permission":`,
			after:  `"delegation-layer-read-only":{"mode":"subagent","permission":`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			mutated := strings.Replace(readOnlyConfigContent, testCase.before, testCase.after, 1)
			if mutated == readOnlyConfigContent {
				t.Fatal("test fixture was not mutated")
			}
			if _, err := projectReadOnlyConfig([]byte(mutated)); err == nil {
				t.Fatal("unsafe agent fallback configuration was accepted")
			}
		})
	}
}

func TestPolicyProjectionRejectsAutomaticCompaction(t *testing.T) {
	mutated := strings.Replace(readOnlyConfigContent, `"auto":false`, `"auto":true`, 1)
	if _, err := projectReadOnlyConfig([]byte(mutated)); err == nil {
		t.Fatal("automatic compaction was accepted")
	}
}

func TestWorkspaceEnvironmentInstallsNativeContainmentPolicy(t *testing.T) {
	home := canonicalTestPath(t, "/tmp/home")
	workspace := testWorkspace(t)
	values, err := environmentForMode([]string{"PATH=/bin", "HOME=" + home}, ModeWorkspaceWrite, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.IsSorted(values) {
		t.Fatalf("environment is not canonical: %q", values)
	}
	if !slices.Contains(values, "OPENCODE_DISABLE_PROJECT_CONFIG=1") {
		t.Fatal("workspace-write environment did not disable project configuration")
	}
	if !slices.Contains(values, "XDG_CONFIG_HOME="+filepath.Join(home, ".config/delegation-layer-opencode")) {
		t.Fatalf("workspace-write environment did not isolate OpenCode config: %q", values)
	}
	config := inlineConfig(t, values)
	permissions, err := workspaceWritePermission(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !samePermissions(config.Permission, permissions) {
		t.Fatalf("global workspace-write permissions=%v", config.Permission)
	}
	agent, ok := config.Agent[workspaceWriteAgentName]
	if !ok || !samePermissions(agent.Permission, permissions) {
		t.Fatalf("workspace-write agent permissions=%v", config.Agent)
	}
	if !jsonFalse(config.LSP) || !jsonFalse(config.Formatter) {
		t.Fatal("workspace-write config enabled native services")
	}
	if !slices.Contains(values, "GIT_CONFIG_NOSYSTEM=1") || !slices.Contains(values, "GIT_CONFIG_GLOBAL=/dev/null") {
		t.Fatal("workspace-write environment did not isolate global Git boundary settings")
	}
}

func TestWorkspaceWritePolicyProjectionAcceptsCompiledEffectiveConfig(t *testing.T) {
	workspace := testWorkspace(t)
	content, err := workspaceWriteConfigContent(workspace)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := projectWorkspaceWriteConfig([]byte(content), workspace)
	if err != nil {
		t.Fatalf("facts=%q err=%v", facts, err)
	}
	if _, err := validateWorkspaceWritePolicyFacts(facts); err != nil {
		t.Fatalf("projected facts failed validation: %v", err)
	}
}

func TestOpenCodePolicyDigestRedactsSecretsButTracksPolicy(t *testing.T) {
	first, err := openCodeConfigDigest([]byte(`{"model":"provider/one","apiKey":"secret-one"}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := openCodeConfigDigest([]byte(`{"model":"provider/one","apiKey":"secret-two"}`))
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("credential-only configuration drift changed digest: %q != %q", first, second)
	}
	third, err := openCodeConfigDigest([]byte(`{"model":"provider/two","apiKey":"secret-one"}`))
	if err != nil {
		t.Fatal(err)
	}
	if first == third {
		t.Fatal("effective model configuration drift did not change digest")
	}
}

func TestOpenCodePermissionRulesPreserveEvaluationOrder(t *testing.T) {
	want := permissionMap{"edit": json.RawMessage(`{"*":"deny","**":"allow"}`)}
	got := permissionMap{"edit": json.RawMessage(`{"**":"allow","*":"deny"}`)}
	if samePermissions(got, want) {
		t.Fatal("reordered overlapping permission rules were accepted")
	}

	first, err := openCodeConfigDigest([]byte(`{"permission":{"edit":{"*":"deny","**":"allow"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := openCodeConfigDigest([]byte(`{"permission":{"edit":{"**":"allow","*":"deny"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("reordered overlapping permission rules did not change the policy digest")
	}
}

func TestOpenCodePolicyProjectionPreservesTopLevelPermissionOrder(t *testing.T) {
	permission := `{"read":"allow","grep":"allow","glob":"allow","list":"allow","*":"deny","edit":"deny","bash":"deny","task":"deny","skill":"deny","lsp":"deny","question":"deny","webfetch":"deny","websearch":"deny","external_directory":"deny","doom_loop":"deny"}`
	native := `{"agent":{"delegation-layer-read-only":{"permission":` + permission + `}},"permission":` + permission + `,"lsp":false,"formatter":false,"compaction":{"auto":false}}`
	if _, err := projectReadOnlyConfig([]byte(native)); err == nil {
		t.Fatal("reordered top-level permission rules were accepted")
	}
}

func TestWorkspaceWritePolicyProjectionRejectsWidening(t *testing.T) {
	workspace := testWorkspace(t)
	content, err := workspaceWriteConfigContent(workspace)
	if err != nil {
		t.Fatal(err)
	}
	widened := strings.Replace(content, `"external_directory":"deny"`, `"external_directory":"allow"`, 2)
	if _, err := projectWorkspaceWriteConfig([]byte(widened), workspace); err == nil {
		t.Fatal("workspace-write external-directory widening was accepted")
	}
}

func TestWorkspaceWritePermissionAllowsGitWorktreeRoot(t *testing.T) {
	root, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeGitConfig(t, root, "[core]\n\trepositoryformatversion = 0\n")
	permissions, err := workspaceWritePermission(root)
	if err != nil {
		t.Fatal(err)
	}
	var editRules map[string]string
	if err := json.Unmarshal(permissions["edit"], &editRules); err != nil {
		t.Fatalf("edit permission is not a path rule: %v", err)
	}
	if editRules["*"] != "deny" || editRules["**"] != "allow" {
		t.Fatalf("edit rules=%v", editRules)
	}
	if len(editRules) != 2 {
		t.Fatalf("unexpected edit rules=%v", editRules)
	}
}

func TestWorkspaceWritePermissionRejectsConfiguredGitWorktreeMismatch(t *testing.T) {
	checkout, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configuredWorktree, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeGitConfig(t, checkout, "[core]\n\trepositoryformatversion = 0\n\tworktree = "+configuredWorktree+"\n")
	resolved, err := openCodeWorktreeRoot(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != configuredWorktree {
		t.Fatalf("resolved worktree=%q want=%q", resolved, configuredWorktree)
	}
	if _, err := workspaceWritePermission(checkout); err == nil {
		t.Fatal("workspace with a mismatched configured Git worktree was accepted")
	}
}

func TestOpenCodeWorktreeResolverHonorsWorktreeConfigEnablement(t *testing.T) {
	checkout, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configuredWorktree, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeGitConfig(t, checkout, "[core]\n\tworktree = "+configuredWorktree+"\n")
	if writeErr := os.WriteFile(filepath.Join(checkout, ".git", "config.worktree"), []byte("[core]\n\tworktree = "+checkout+"\n"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	resolved, err := openCodeWorktreeRoot(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != configuredWorktree {
		t.Fatalf("disabled worktreeConfig resolved=%q want=%q", resolved, configuredWorktree)
	}
	if writeErr := os.WriteFile(filepath.Join(checkout, ".git", "config"), []byte("[core]\n\tworktree = "+configuredWorktree+"\n[extensions]\n\tworktreeConfig = true\n"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	resolved, err = openCodeWorktreeRoot(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != checkout {
		t.Fatalf("enabled worktreeConfig resolved=%q want=%q", resolved, checkout)
	}
}

func TestOpenCodeWorktreeResolverParsesCommentedSections(t *testing.T) {
	checkout, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configuredWorktree, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeGitConfig(t, checkout, "[core] # native comment\n\tworktree = "+configuredWorktree+"\n")
	resolved, err := openCodeWorktreeRoot(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != configuredWorktree {
		t.Fatalf("commented core section resolved=%q want=%q", resolved, configuredWorktree)
	}
}

func TestOpenCodeWorktreeResolverParsesUnspacedGitComments(t *testing.T) {
	checkout, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configuredWorktree, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeGitConfig(t, checkout, "[core]\n\tworktree = "+configuredWorktree+"# trailing comment\n")
	resolved, err := openCodeWorktreeRoot(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != configuredWorktree {
		t.Fatalf("unspaced Git comment resolved=%q want=%q", resolved, configuredWorktree)
	}
}

func TestOpenCodeWorktreeResolverAcceptsValuelessGitBooleans(t *testing.T) {
	checkout, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeGitConfig(t, checkout, "[core]\n\tlogallrefupdates\n[extensions]\n\tworktreeConfig\n")
	if writeErr := os.WriteFile(filepath.Join(checkout, ".git", "config.worktree"), []byte("[core]\n\tworktree = "+checkout+"\n"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	resolved, err := openCodeWorktreeRoot(checkout)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != checkout {
		t.Fatalf("valueless Git booleans resolved=%q want=%q", resolved, checkout)
	}
}

func TestWorkspaceWritePermissionRejectsGitConfigIncludes(t *testing.T) {
	checkout, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeGitConfig(t, checkout, "[core]\n\trepositoryformatversion = 0\n")
	configPath := filepath.Join(checkout, ".git", "config")
	file, err := os.OpenFile(configPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString("\n[include]\n\tpath = included-config\n")
	closeErr := file.Close()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if _, err := workspaceWritePermission(checkout); err == nil {
		t.Fatal("Git config include was accepted")
	}
}

func TestWorkspaceWritePermissionRejectsNestedGitWorkspace(t *testing.T) {
	root, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeGitConfig(t, root, "[core]\n\trepositoryformatversion = 0\n")
	workspace := filepath.Join(root, "work")
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceWritePermission(workspace); err == nil {
		t.Fatal("nested Git workspace-write profile was accepted")
	}
}

func TestWorkspaceWritePolicyProjectionRejectsDifferentWorkspace(t *testing.T) {
	workspace, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeGitConfig(t, workspace, "[core]\n\trepositoryformatversion = 0\n")
	content, err := workspaceWriteConfigContent(workspace)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(workspace, "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := projectWorkspaceWriteConfig([]byte(content), other); err == nil {
		t.Fatal("workspace policy for a different directory was accepted")
	}
}

func TestWorkspaceWritePermissionRejectsSymlink(t *testing.T) {
	workspace := testWorkspace(t)
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(workspace, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceWritePermission(workspace); err == nil {
		t.Fatal("workspace symlink was accepted")
	}
}

func TestWorkspaceWritePermissionRejectsGlobMetacharacters(t *testing.T) {
	root, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeGitConfig(t, root, "[core]\n\trepositoryformatversion = 0\n")
	workspace := filepath.Join(root, "work*")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceWritePermission(workspace); err == nil {
		t.Fatal("workspace glob metacharacter was accepted")
	}
}

func TestPolicyProjectionRejectsEnabledMCP(t *testing.T) {
	base := `{"agent":{"delegation-layer-read-only":{"permission":{"*":"deny","read":"allow","grep":"allow","glob":"allow","list":"allow","edit":"deny","bash":"deny","task":"deny","skill":"deny","lsp":"deny","question":"deny","webfetch":"deny","websearch":"deny","external_directory":"deny","doom_loop":"deny"}}},"permission":{"*":"deny","read":"allow","grep":"allow","glob":"allow","list":"allow","edit":"deny","bash":"deny","task":"deny","skill":"deny","lsp":"deny","question":"deny","webfetch":"deny","websearch":"deny","external_directory":"deny","doom_loop":"deny"},"lsp":false,"formatter":false}`
	withMCP := strings.TrimSuffix(base, "}") + `,"mcp":{"local":{"type":"local","command":["unsafe-helper"]}}}`
	if _, err := projectReadOnlyConfig([]byte(withMCP)); err == nil {
		t.Fatal("enabled MCP server was accepted")
	}
}

func TestPolicyProjectionAllowsExplicitlyDisabledMCP(t *testing.T) {
	base := strings.TrimSuffix(readOnlyConfigContent, "}") + `,"mcp":{"local":{"type":"local","command":["helper"],"enabled":false}}}`
	if _, err := projectReadOnlyConfig([]byte(base)); err != nil {
		t.Fatalf("disabled MCP server was rejected: %v", err)
	}
}

func permissionString(t *testing.T, permissions permissionMap, key string) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(permissions[key], &value); err != nil {
		t.Fatalf("permission[%q] is not a string: %v", key, err)
	}
	return value
}

func inlineConfig(t *testing.T, values []string) inlinePermissionConfig {
	t.Helper()
	var found string
	for _, value := range values {
		if strings.HasPrefix(value, "OPENCODE_CONFIG_CONTENT=") {
			found = strings.TrimPrefix(value, "OPENCODE_CONFIG_CONTENT=")
		}
	}
	if found == "" {
		t.Fatal("environment did not provide inline permission config")
	}
	var config inlinePermissionConfig
	if decodeErr := json.Unmarshal([]byte(found), &config); decodeErr != nil {
		t.Fatalf("inline config is not JSON: %v", decodeErr)
	}
	return config
}

func assertPermissionStrings(t *testing.T, permissions permissionMap, keys []string, want string) {
	t.Helper()
	for _, key := range keys {
		if got := permissionString(t, permissions, key); got != want {
			t.Fatalf("permission[%q]=%q want=%q", key, got, want)
		}
	}
}

func testWorkspace(t *testing.T) string {
	t.Helper()
	workspace, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return workspace
}

func canonicalTestPath(t *testing.T, path string) string {
	t.Helper()
	canonical, err := config.CanonicalizePath(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func writeGitConfig(t *testing.T, directory, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(directory, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".git", "config"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
