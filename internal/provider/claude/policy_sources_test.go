package claude

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type claudePolicyFixture struct {
	files map[string][]byte
	calls []string
}

func (fixture *claudePolicyFixture) read(path string) (commonprovider.SourceBytes, error) {
	fixture.calls = append(fixture.calls, path)
	data, present := fixture.files[path]
	if !present {
		return commonprovider.SourceBytes{Path: path}, nil
	}
	return commonprovider.SourceBytes{Path: path, Present: true, Data: append([]byte(nil), data...)}, nil
}

func claudePolicyTestRequest(workspace string) task.TaskRecord {
	return task.TaskRecord{
		RootID:       strings.Repeat("1", 32),
		TaskID:       strings.Repeat("2", 32),
		Provider:     Provider,
		Mode:         Mode,
		CanonicalCwd: workspace,
		RequestedConfig: task.TaskConfig{
			Permission: Mode,
			Budget:     time.Second.String(),
		},
		BudgetNanos: int64(time.Second),
		BriefLength: 1,
	}
}

func claudePolicyTestEnvironment(root string) (profileEnvironment, string) {
	home := filepath.Join(root, "home")
	return profileEnvironment{
		Home:       home,
		ClaudeHome: filepath.Join(home, ".claude"),
		Values:     []string{storageBackendPin},
	}, home
}

func TestResolvePolicySourcesRequiresFixedStorageBackendPin(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	workspace := filepath.Join(root, "workspace")
	environment, _ := claudePolicyTestEnvironment(root)
	environment.Values = nil
	fixture := &claudePolicyFixture{files: map[string][]byte{}}

	_, err := resolvePolicySourcesWithReaderAndUsername(claudePolicyTestRequest(workspace), environment, fixture.read, "fixture-user")
	if !errors.Is(err, ErrUnsupportedProfile) || !strings.Contains(err.Error(), "fixed StorageV5 false pin is required") {
		t.Fatalf("unpinned empty policy inventory was accepted: %v", err)
	}
	if len(fixture.calls) != 0 {
		t.Fatalf("unpinned resolver read policy sources: %v", fixture.calls)
	}
}

func TestResolvePolicySourcesInventoriesAbsentSourcesAndManagedPaths(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	workspace := filepath.Join(root, "checkout", "nested")
	request := claudePolicyTestRequest(workspace)
	environment, _ := claudePolicyTestEnvironment(root)
	fixture := &claudePolicyFixture{files: map[string][]byte{}}

	sources, err := resolvePolicySourcesWithReaderAndUsername(request, environment, fixture.read, "fixture-user")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) == 0 {
		t.Fatal("source inventory is empty")
	}
	for index, source := range sources {
		if source.Present || source.SHA256 != "" {
			t.Fatalf("source %d was not absent: %+v", index, source)
		}
		if index > 0 && (sources[index-1].Path > source.Path || (sources[index-1].Path == source.Path && sources[index-1].Kind >= source.Kind)) {
			t.Fatalf("sources are not ordered: previous=%+v current=%+v", sources[index-1], source)
		}
	}
	wantPaths := []string{
		filepath.Join(environment.ClaudeHome, ".config.json"),
		filepath.Join(environment.Home, ".claude.json"),
		filepath.Join(environment.ClaudeHome, ".credentials.json"),
		filepath.Join(environment.ClaudeHome, "remote-settings.json"),
		filepath.Join(environment.ClaudeHome, "remote-settings-helper-consent"),
		filepath.Join(environment.ClaudeHome, "remote-settings-consent.json"),
		filepath.Join(environment.ClaudeHome, "remote-settings.json.signature.json"),
		filepath.Join(environment.ClaudeHome, "remote-settings.json.signature-iat.json"),
		filepath.Join(claudeManagedRoot, "managed-settings.json"),
		filepath.Join(claudeManagedRoot, "managed-settings.d"),
		filepath.Join(claudeManagedRoot, "managed-mcp.json"),
		filepath.Join(claudeManagedPreferences, "fixture-user", "com.anthropic.claudecode.plist"),
		filepath.Join(claudeManagedPreferences, "com.anthropic.claudecode.plist"),
	}
	for _, path := range wantPaths {
		if !slices.ContainsFunc(sources, func(source task.PolicySourceDigest) bool { return source.Path == path }) {
			t.Fatalf("source path missing from inventory: %s", path)
		}
	}
	credentialsPath := filepath.Join(environment.ClaudeHome, ".credentials.json")
	if slices.Contains(fixture.calls, credentialsPath) {
		t.Fatal("plaintext credentials fallback was read through the policy source reader")
	}
	policy := task.PolicyDetails{
		ProfileRevision: "claude-policy-test",
		RuntimeSHA256:   strings.Repeat("a", 64),
		Workspace:       workspace,
		Sources:         sources,
	}
	if err := task.ValidateEffectiveConfig(task.EffectiveConfig{Digest: strings.Repeat("b", 64), Policy: &policy}); err != nil {
		t.Fatalf("source inventory violates task policy contract: %v", err)
	}
}

func TestLegacyGlobalConfigTakesPrecedenceOverCurrentFallback(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	workspace := filepath.Join(root, "workspace")
	environment, _ := claudePolicyTestEnvironment(root)
	legacyPath := filepath.Join(environment.ClaudeHome, ".config.json")
	globalPath := filepath.Join(environment.Home, ".claude.json")
	secret := "fixture-primary-api-key"
	fixture := &claudePolicyFixture{files: map[string][]byte{
		legacyPath: []byte(`{"oauthAccount":{"email":"fixture@example.invalid"}}`),
		globalPath: []byte(`{"primaryApiKey":"` + secret + `"}`),
	}}

	sources, err := resolvePolicySourcesWithReaderAndUsername(claudePolicyTestRequest(workspace), environment, fixture.read, "fixture-user")
	if err != nil {
		t.Fatal(err)
	}
	legacyIndex := slices.IndexFunc(sources, func(source task.PolicySourceDigest) bool { return source.Kind == claudeLegacyGlobalConfigKind })
	if legacyIndex < 0 || !sources[legacyIndex].Present {
		t.Fatalf("legacy source was not selected: %+v", sources)
	}
	if slices.ContainsFunc(sources, func(source task.PolicySourceDigest) bool { return source.Kind == claudeGlobalConfigKind }) {
		t.Fatalf("inactive current fallback was observed: %+v", sources)
	}
	if strings.Contains(sources[legacyIndex].SHA256, secret) || sources[legacyIndex].SHA256 == task.ComputeSHA256(fixture.files[legacyPath]) {
		t.Fatalf("source digest exposed or hashed raw source: %+v", sources[legacyIndex])
	}
	if slices.Contains(fixture.calls, globalPath) {
		t.Fatal("inactive current fallback was read")
	}
}

func TestPrimaryAPIKeyAndExecutableHelperRefuseBeforeDigest(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	workspace := filepath.Join(root, "workspace")
	environment, _ := claudePolicyTestEnvironment(root)
	globalPath := filepath.Join(environment.Home, ".claude.json")
	for _, data := range []string{
		`{"primaryApiKey":"fixture-secret-value"}`,
		`{"apiKeyHelper":"/tmp/fixture-helper"}`,
	} {
		fixture := &claudePolicyFixture{files: map[string][]byte{globalPath: []byte(data)}}
		_, err := resolvePolicySourcesWithReaderAndUsername(claudePolicyTestRequest(workspace), environment, fixture.read, "fixture-user")
		if !errors.Is(err, ErrUnsupportedProfile) {
			t.Fatalf("global alternate-auth source was accepted: %v", err)
		}
		if strings.Contains(err.Error(), "fixture-secret-value") || strings.Contains(err.Error(), "fixture-helper") {
			t.Fatalf("secret or executable detail escaped refusal: %v", err)
		}
	}
}

func TestGlobalStorageV5AndRemoteFeatureCachesAreShapeOnlyWithFixedFalsePin(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	workspace := filepath.Join(root, "workspace")
	environment, _ := claudePolicyTestEnvironment(root)
	globalPath := filepath.Join(environment.Home, ".claude.json")
	for _, testCase := range []struct {
		name   string
		data   string
		marker string
	}{
		{
			name:   "storage latch feature",
			data:   `{"cachedGrowthBookFeatures":{"tengu_hover_rest":true}}`,
			marker: "tengu_hover_rest",
		},
		{
			name:   "feature cache timestamp",
			data:   `{"cachedGrowthBookFeaturesAt":1234567890}`,
			marker: "1234567890",
		},
		{
			name:   "experiment feature cache",
			data:   `{"cachedExperimentFeatures":["tengu_hover_rest"]}`,
			marker: "tengu_hover_rest",
		},
		{
			name:   "experiment data cache",
			data:   `{"cachedExperimentData":{"tengu_hover_rest":{"value":true}}}`,
			marker: "tengu_hover_rest",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := &claudePolicyFixture{files: map[string][]byte{globalPath: []byte(testCase.data)}}
			sources, err := resolvePolicySourcesWithReaderAndUsername(claudePolicyTestRequest(workspace), environment, fixture.read, "fixture-user")
			if err != nil {
				t.Fatalf("native feature cache was rejected with fixed false pin: %v", err)
			}
			index := slices.IndexFunc(sources, func(source task.PolicySourceDigest) bool {
				return source.Path == globalPath && source.Kind == claudeGlobalConfigKind
			})
			if index < 0 || !sources[index].Present || sources[index].SHA256 == "" {
				t.Fatalf("global feature cache was not recorded as a shape digest: %+v", sources)
			}
			if sources[index].SHA256 == task.ComputeSHA256([]byte(testCase.data)) || strings.Contains(sources[index].SHA256, testCase.marker) {
				t.Fatalf("feature-cache value was included in the digest: %+v", sources[index])
			}
		})
	}
}

func TestRemoteFeatureFlagRemainsIndependentlyCheckedWithFixedFalsePin(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	workspace := filepath.Join(root, "workspace")
	environment, _ := claudePolicyTestEnvironment(root)
	remotePath := filepath.Join(environment.ClaudeHome, "remote-settings.json")
	fixture := &claudePolicyFixture{files: map[string][]byte{
		remotePath: []byte(`{"tengu_hover_rest":true}`),
	}}

	_, err := resolvePolicySourcesWithReaderAndUsername(claudePolicyTestRequest(workspace), environment, fixture.read, "fixture-user")
	if !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("non-empty remote policy cache was accepted: %v", err)
	}
	if strings.Contains(err.Error(), "tengu_hover_rest") {
		t.Fatalf("remote feature-cache value escaped refusal: %v", err)
	}
}

type plaintextCredentialsFallbackSetup func(*testing.T, profileEnvironment, string)

func TestPlaintextCredentialsFallbackIsMetadataOnlyAndFailClosed(t *testing.T) {
	for _, testCase := range plaintextCredentialsFallbackCases() {
		t.Run(testCase.name, func(t *testing.T) {
			testPlaintextCredentialsFallback(t, testCase.setup)
		})
	}
}

func plaintextCredentialsFallbackCases() []struct {
	name  string
	setup plaintextCredentialsFallbackSetup
} {
	return []struct {
		name  string
		setup plaintextCredentialsFallbackSetup
	}{
		{name: "present regular file", setup: setupPresentCredentialsFile},
		{name: "malformed regular file", setup: setupMalformedCredentialsFile},
		{name: "symlink", setup: setupCredentialsSymlink},
		{name: "unreadable parent", setup: setupUnreadableCredentialsParent},
	}
}

func setupPresentCredentialsFile(t *testing.T, _ profileEnvironment, path string) {
	t.Helper()
	writeCredentialsFixture(t, path, []byte(`{"claudeAiOauth":{"accessToken":"fixture-secret"}}`))
}

func setupMalformedCredentialsFile(t *testing.T, _ profileEnvironment, path string) {
	t.Helper()
	writeCredentialsFixture(t, path, []byte("fixture-secret-not-json"))
}

func setupCredentialsSymlink(t *testing.T, _ profileEnvironment, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(path), "credentials-target.json")
	writeCredentialsFixture(t, target, []byte(`{"claudeAiOauth":{"accessToken":"fixture-secret"}}`))
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func setupUnreadableCredentialsParent(t *testing.T, environment profileEnvironment, _ string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(environment.ClaudeHome), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(environment.ClaudeHome, []byte("fixture-parent"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeCredentialsFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func testPlaintextCredentialsFallback(t *testing.T, setup plaintextCredentialsFallbackSetup) {
	t.Helper()
	root := filepath.Clean(t.TempDir())
	workspace := filepath.Join(root, "workspace")
	environment, _ := claudePolicyTestEnvironment(root)
	credentialsPath := filepath.Join(environment.ClaudeHome, ".credentials.json")
	setup(t, environment, credentialsPath)
	fixture := &claudePolicyFixture{files: map[string][]byte{}}

	_, err := resolvePolicySourcesWithReaderAndUsername(claudePolicyTestRequest(workspace), environment, fixture.read, "fixture-user")
	if !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("plaintext credentials fallback was accepted: %v", err)
	}
	if strings.Contains(err.Error(), "fixture-secret") || strings.Contains(err.Error(), "fixture-parent") {
		t.Fatalf("credential contents escaped metadata-only refusal: %v", err)
	}
	if slices.Contains(fixture.calls, credentialsPath) {
		t.Fatal("plaintext credentials fallback was read through the policy source reader")
	}
}

func TestGlobalExecutableControlsRefuseBeforeDigest(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	workspace := filepath.Join(root, "workspace")
	environment, _ := claudePolicyTestEnvironment(root)
	globalPath := filepath.Join(environment.Home, ".claude.json")
	for _, data := range []string{
		`{"awsAuthRefresh":"/tmp/fixture-helper"}`,
		`{"awsCredentialExport":"/tmp/fixture-export"}`,
		`{"gcpAuthRefresh":"/tmp/fixture-gcp"}`,
		`{"otelHeadersHelper":"/tmp/fixture-otel"}`,
		`{"proxyAuthHelper":"/tmp/fixture-proxy"}`,
		`{"statusLine":{"type":"command","command":"/tmp/fixture-status"}}`,
		`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/tmp/fixture-hook"}]}]}}`,
	} {
		fixture := &claudePolicyFixture{files: map[string][]byte{globalPath: []byte(data)}}
		_, err := resolvePolicySourcesWithReaderAndUsername(claudePolicyTestRequest(workspace), environment, fixture.read, "fixture-user")
		if !errors.Is(err, ErrUnsupportedProfile) {
			t.Fatalf("global executable or MCP source was accepted: %v", err)
		}
		for _, marker := range []string{"fixture-helper", "fixture-export", "fixture-gcp", "fixture-otel", "fixture-proxy", "fixture-status", "fixture-hook"} {
			if strings.Contains(err.Error(), marker) {
				t.Fatalf("executable or command value escaped refusal: %v", err)
			}
		}
	}
}

func TestOrdinaryMCPIsBypassedByCertifiedProfile(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	workspace := filepath.Join(root, "checkout", "nested")
	environment, _ := claudePolicyTestEnvironment(root)
	globalPath := filepath.Join(environment.Home, ".claude.json")
	projectPath := filepath.Join(workspace, ".mcp.json")
	globalData := []byte(`{"mcpServers":{"fixture":{"command":"/tmp/fixture-mcp","env":{"primaryApiKey":"fixture-secret-a","apiKeyHelper":"/tmp/fixture-helper-a"},"headers":{"authorization":"fixture-header-a"}}}}`)
	projectData := []byte(`{"mcpServers":{"fixture":{"command":"/tmp/project-mcp","env":{"primaryApiKey":"fixture-secret-b","apiKeyHelper":"/tmp/project-helper"},"headers":{"authorization":"project-header"}}}}`)
	fixture := &claudePolicyFixture{files: map[string][]byte{
		globalPath:  globalData,
		projectPath: projectData,
	}}

	sources, err := resolvePolicySourcesWithReaderAndUsername(claudePolicyTestRequest(workspace), environment, fixture.read, "fixture-user")
	if err != nil {
		t.Fatalf("ordinary MCP configuration was rejected under the certified strict profile: %v", err)
	}
	for _, want := range []struct {
		path string
		kind string
	}{
		{path: globalPath, kind: claudeGlobalConfigKind},
		{path: projectPath, kind: claudeProjectMCPKind},
	} {
		index := slices.IndexFunc(sources, func(source task.PolicySourceDigest) bool {
			return source.Path == want.path && source.Kind == want.kind
		})
		if index < 0 || !sources[index].Present || sources[index].SHA256 == "" {
			t.Fatalf("ordinary MCP source was not recorded as a shape digest: %+v", sources)
		}
	}

	globalShape, err := inspectClaudeGlobalConfig(globalData)
	if err != nil {
		t.Fatal(err)
	}
	globalShapeChanged, err := inspectClaudeGlobalConfig([]byte(`{"mcpServers":{"fixture":{"command":"/tmp/other-mcp","env":{"primaryApiKey":"fixture-secret-z","apiKeyHelper":"/tmp/other-helper"},"headers":{"authorization":"other-header"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(globalShape, globalShapeChanged) {
		t.Fatalf("ordinary MCP values changed the non-secret shape projection: %s != %s", globalShape, globalShapeChanged)
	}
	if strings.Contains(string(globalShape), "fixture-secret") || strings.Contains(string(globalShape), "fixture-helper") {
		t.Fatalf("ordinary MCP values escaped the shape projection: %s", globalShape)
	}
}

func TestGlobalControlsOutsideOrdinaryMCPStillRefuse(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	workspace := filepath.Join(root, "workspace")
	environment, _ := claudePolicyTestEnvironment(root)
	globalPath := filepath.Join(environment.Home, ".claude.json")
	for _, data := range []string{
		`{"mcpServers":{"fixture":{"command":"/tmp/fixture-mcp","env":{"apiKeyHelper":"fixture-nested-helper"}}},"apiKeyHelper":"/tmp/top-level-helper"}`,
		`{"managedMcpServers":{"fixture":{"command":"/tmp/managed-mcp"}}}`,
		`{"allowedMcpServers":["fixture"]}`,
		`{"allowedTools":["Bash"]}`,
		`{"projects":{"workspace":{"managedMcpServers":{"fixture":{"command":"/tmp/nested-managed-mcp"}}}}}`,
		`{"projects":{"workspace":{"allowedTools":["Bash"]}}}`,
		`{"McpServers":{"fixture":{"command":"/tmp/mcp-case"}}}`,
	} {
		fixture := &claudePolicyFixture{files: map[string][]byte{globalPath: []byte(data)}}
		_, err := resolvePolicySourcesWithReaderAndUsername(claudePolicyTestRequest(workspace), environment, fixture.read, "fixture-user")
		if !errors.Is(err, ErrUnsupportedProfile) {
			t.Fatalf("active global policy outside ordinary MCP was accepted: %v", err)
		}
		for _, marker := range []string{"fixture-nested-helper", "top-level-helper", "managed-mcp", "nested-managed-mcp", "mcp-case"} {
			if strings.Contains(err.Error(), marker) {
				t.Fatalf("policy value escaped refusal: %v", err)
			}
		}
	}
}

func TestSkillUsageTelemetryDoesNotActivateExecutableControls(t *testing.T) {
	data := []byte(`{"skillUsage":{"statusLine":{"usageCount":2,"lastUsedAt":123},"apiKeyHelper":{"usageCount":1,"lastUsedAt":456},"hooks":{"usageCount":1,"lastUsedAt":789},"allowedTools":{"usageCount":3,"lastUsedAt":321},"managedMcpServers":{"usageCount":4,"lastUsedAt":654}}}`)
	shape, err := inspectClaudeGlobalConfig(data)
	if err != nil {
		t.Fatalf("skillUsage bookkeeping was treated as executable policy: %v", err)
	}
	changedShape, err := inspectClaudeGlobalConfig([]byte(`{"skillUsage":{"statusLine":{"usageCount":200,"lastUsedAt":999},"apiKeyHelper":{"usageCount":100,"lastUsedAt":654},"hooks":{"usageCount":300,"lastUsedAt":987},"allowedTools":{"usageCount":30,"lastUsedAt":3210},"managedMcpServers":{"usageCount":40,"lastUsedAt":6540}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(shape, changedShape) {
		t.Fatalf("skillUsage telemetry values changed the shape projection: %s != %s", shape, changedShape)
	}

	for _, data := range []string{
		`{"statusLine":{"type":"command","command":"/tmp/top-level-status"}}`,
		`{"projects":{"skillUsage":{"statusLine":{"type":"command","command":"/tmp/nested-status"}}}}`,
	} {
		if _, err := inspectClaudeGlobalConfig([]byte(data)); err == nil {
			t.Fatalf("active statusLine policy was accepted: %s", data)
		}
	}
}

func TestGlobalEmptyProjectControlsAreShapeOnly(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	workspace := filepath.Join(root, "workspace")
	environment, _ := claudePolicyTestEnvironment(root)
	globalPath := filepath.Join(environment.Home, ".claude.json")
	for _, data := range []string{
		`{"mcpServers":{}}`,
		`{"projects":{"workspace":{"mcpServers":{},"allowedTools":[],"enabledMcpjsonServers":[],"disabledMcpjsonServers":[]}}}`,
	} {
		fixture := &claudePolicyFixture{files: map[string][]byte{globalPath: []byte(data)}}
		if _, err := resolvePolicySourcesWithReaderAndUsername(claudePolicyTestRequest(workspace), environment, fixture.read, "fixture-user"); err != nil {
			t.Fatalf("empty global MCP or tool controls were rejected: %v", err)
		}
	}
}

func TestClaudeSettingsAndMCPControls(t *testing.T) {
	for _, testCase := range []struct {
		name string
		data string
		want bool
	}{
		{name: "benign appearance", data: `{"theme":"dark","attribution":{"commit":"fixture"}}`, want: true},
		{name: "hooks", data: `{"hooks":{"SessionStart":[]}}`},
		{name: "permissions", data: `{"permissions":{"defaultMode":"dontAsk"}}`},
		{name: "nested environment", data: `{"attribution":{"env":{"PATH":"/tmp"}}}`},
		{name: "unknown top-level", data: `{"futurePolicyField":true}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := inspectClaudeSettings([]byte(testCase.data))
			if (err == nil) != testCase.want {
				t.Fatalf("inspect settings error=%v", err)
			}
		})
	}
	for _, testCase := range []struct {
		name string
		data string
		want bool
	}{
		{name: "empty records", data: `{"version":1,"records":{}}`, want: true},
		{name: "valid record", data: `{"version":1,"records":{"org":{"accountUuid":"account","dangerousSettingsHash":"hash","updatedAt":1}}}`, want: true},
		{name: "wrong version", data: `{"version":2,"records":{}}`},
		{name: "unknown field", data: `{"version":1,"records":{},"future":true}`},
		{name: "missing record field", data: `{"version":1,"records":{"org":{"accountUuid":"account"}}}`},
	} {
		t.Run("consent-records-"+testCase.name, func(t *testing.T) {
			_, err := inspectClaudeRemoteConsentRecords([]byte(testCase.data))
			if (err == nil) != testCase.want {
				t.Fatalf("inspect consent records error=%v", err)
			}
		})
	}
	for _, testCase := range []struct {
		name string
		data string
		want bool
	}{
		{name: "empty object", data: `{}`, want: true},
		{name: "empty server map", data: `{"mcpServers":{}}`, want: true},
		{name: "server command", data: `{"mcpServers":{"fixture":{"command":"/bin/true"}}}`, want: true},
		{name: "unknown field", data: `{"servers":{}}`},
	} {
		t.Run("mcp-"+testCase.name, func(t *testing.T) {
			_, err := inspectClaudeMCP([]byte(testCase.data))
			if (err == nil) != testCase.want {
				t.Fatalf("inspect MCP error=%v", err)
			}
		})
	}
}

func TestRemoteSettingsCacheRequiresEmptyCompatibleSnapshot(t *testing.T) {
	for _, testCase := range []struct {
		name string
		data string
		want bool
	}{
		{name: "empty", data: `{}`, want: true},
		{name: "schema marker", data: `{"$schema":"fixture"}`, want: true},
		{name: "model control", data: `{"model":"fixture"}`},
		{name: "managed MCP control", data: `{"managedMcpServers":{}}`},
		{name: "invalid root", data: `[]`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := inspectClaudeRemoteSettings([]byte(testCase.data))
			if (err == nil) != testCase.want {
				t.Fatalf("inspect remote cache error=%v", err)
			}
		})
	}
	for _, testCase := range []struct {
		name string
		data []byte
		want bool
	}{
		{name: "absent", want: true},
		{name: "empty present", data: nil, want: true},
		{name: "opaque consent", data: []byte("fixture-consent"), want: false},
	} {
		t.Run("sidecar-"+testCase.name, func(t *testing.T) {
			source := commonprovider.SourceBytes{Path: "/fixture/remote-settings-helper-consent", Present: testCase.name != "absent", Data: testCase.data}
			_, err := observeClaudeOpaqueEmptySource(source, claudeRemoteConsentKind, true)
			if (err == nil) != testCase.want {
				t.Fatalf("observe sidecar error=%v", err)
			}
		})
	}
}

func TestPresentManagedSourceAndAlternateSelectorRefuse(t *testing.T) {
	root := filepath.Clean(t.TempDir())
	workspace := filepath.Join(root, "workspace")
	environment, _ := claudePolicyTestEnvironment(root)
	managedPath := filepath.Join(claudeManagedRoot, "managed-settings.json")
	fixture := &claudePolicyFixture{files: map[string][]byte{managedPath: []byte(`{"hooks":{"SessionStart":[]}}`)}}
	_, err := resolvePolicySourcesWithReaderAndUsername(claudePolicyTestRequest(workspace), environment, fixture.read, "fixture-user")
	if !errors.Is(err, ErrUnsupportedProfile) || strings.Contains(err.Error(), "SessionStart") {
		t.Fatalf("managed source handling leaked or accepted policy: %v", err)
	}

	environment.Values = []string{"CLAUDE_CODE_REMOTE_SETTINGS_PATH=/tmp/fixture"}
	fixture.files = map[string][]byte{}
	_, err = resolvePolicySourcesWithReaderAndUsername(claudePolicyTestRequest(workspace), environment, fixture.read, "fixture-user")
	if !errors.Is(err, ErrUnsupportedProfile) || !strings.Contains(err.Error(), "alternate Claude environment selectors") {
		t.Fatalf("alternate policy selector was accepted: %v", err)
	}
}

func TestPolicySourceReaderRejectsPathMismatch(t *testing.T) {
	request := claudePolicyTestRequest("/fixture/workspace")
	environment := profileEnvironment{Home: "/fixture/home", ClaudeHome: "/fixture/home/.claude", Values: []string{storageBackendPin}}
	reader := func(path string) (commonprovider.SourceBytes, error) {
		return commonprovider.SourceBytes{Path: path + ".other", Present: true, Data: []byte(`{}`)}, nil
	}
	_, err := resolvePolicySourcesWithReaderAndUsername(request, environment, reader, "fixture-user")
	if !errors.Is(err, ErrUnsupportedProfile) || !strings.Contains(err.Error(), "returned") {
		t.Fatalf("reader path mismatch was accepted: %v", err)
	}
}
