package pi

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestInspectModelsConfigRecordsCredentialBlindPolicyDigest(t *testing.T) {
	agentDir := canonicalTempDir(t)
	path := filepath.Join(agentDir, "models.json")
	contents := `{"providers":{"openai":{"apiKey":"literal","headers":{"X-Test":"value"}}}}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	source, err := inspectModelsConfig(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	if source.Path != path || source.Kind != piModelsConfigKind || !source.Present {
		t.Fatalf("unexpected source metadata: %+v", source)
	}
	if source.SHA256 == "" || source.SHA256 == task.ComputeSHA256([]byte(contents)) {
		t.Fatalf("source digest appears to hash raw model configuration: %q", source.SHA256)
	}
	changedCredentials := `{"providers":{"openai":{"apiKey":"different-secret","headers":{"X-Test":"different-value"}}}}`
	if writeErr := os.WriteFile(path, []byte(changedCredentials), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	changed, err := inspectModelsConfig(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	if changed.SHA256 != source.SHA256 {
		t.Fatal("credential bytes changed the model policy projection")
	}
	changedEndpoint := `{"providers":{"openai":{"apiKey":"different-secret","baseUrl":"https://different.example.invalid"}}}`
	if writeErr := os.WriteFile(path, []byte(changedEndpoint), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	endpoint, err := inspectModelsConfig(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.SHA256 == source.SHA256 {
		t.Fatal("model endpoint change did not alter policy projection")
	}
}

func TestInspectModelsConfigAllowsAbsentFile(t *testing.T) {
	source, err := inspectModelsConfig(canonicalTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if source.Present || source.SHA256 != "" {
		t.Fatalf("absent source=%+v", source)
	}
}

func TestInspectModelsConfigRejectsCommandValues(t *testing.T) {
	cases := map[string]string{
		"api key": `{"providers":{"openai":{"apiKey":"!printf secret"}}}`,
		"header":  `{"providers":{"openai":{"headers":{"Authorization":"!printf secret"}}}}`,
		"nested":  `{"providers":{"openai":{"modelOverrides":{"model":{"samplingParams":{"custom":"!touch /tmp/nope"}}}}}}`,
	}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			agentDir := canonicalTempDir(t)
			if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := inspectModelsConfig(agentDir); !errors.Is(err, ErrUnsupportedProfile) {
				t.Fatalf("error=%v want unsupported profile", err)
			}
		})
	}
}

func TestInspectSettingsConfigTracksDefaultsWithoutCredentialBytes(t *testing.T) {
	agentDir := canonicalTempDir(t)
	path := filepath.Join(agentDir, "settings.json")
	firstContents := `{"defaultProvider":"openai","defaultModel":"gpt-5","defaultThinkingLevel":"high","apiKey":"secret-one"}`
	if err := os.WriteFile(path, []byte(firstContents), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := inspectSettingsConfig(path, piGlobalSettingsConfigKind)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Present || first.SHA256 == "" || first.SHA256 == task.ComputeSHA256([]byte(firstContents)) {
		t.Fatalf("unexpected settings source=%+v", first)
	}
	secondContents := `{"defaultProvider":"openai","defaultModel":"gpt-5","defaultThinkingLevel":"high","apiKey":"secret-two"}`
	if writeErr := os.WriteFile(path, []byte(secondContents), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	second, err := inspectSettingsConfig(path, piGlobalSettingsConfigKind)
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 {
		t.Fatal("credential bytes changed the settings projection")
	}
	changedContents := `{"defaultProvider":"openai","defaultModel":"gpt-5.1","defaultThinkingLevel":"high","apiKey":"secret-two"}`
	if writeErr := os.WriteFile(path, []byte(changedContents), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	changed, err := inspectSettingsConfig(path, piGlobalSettingsConfigKind)
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 == changed.SHA256 {
		t.Fatal("default model change did not alter settings projection")
	}
}

func TestInspectAuthConfigRecordsShapeWithoutCredentialBytes(t *testing.T) {
	agentDir := canonicalTempDir(t)
	path := filepath.Join(agentDir, "auth.json")
	firstContents := `{"openai":{"type":"api_key","key":"secret-one"},"anthropic":{"type":"oauth","access":"access-one","refresh":"refresh-one","expires":1}}`
	if err := os.WriteFile(path, []byte(firstContents), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := inspectAuthConfig(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Present || first.SHA256 == "" {
		t.Fatalf("unexpected auth source=%+v", first)
	}
	secondContents := `{"openai":{"type":"api_key","key":"secret-two"},"anthropic":{"type":"oauth","access":"access-two","refresh":"refresh-two","expires":2}}`
	if writeErr := os.WriteFile(path, []byte(secondContents), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	second, err := inspectAuthConfig(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 {
		t.Fatal("credential bytes changed the nonsecret auth projection")
	}
}

func TestInspectAuthConfigRejectsCommandValuedAPIKey(t *testing.T) {
	agentDir := canonicalTempDir(t)
	contents := `{"openai":{"type":"api_key","key":"!printf secret"}}`
	if err := os.WriteFile(filepath.Join(agentDir, "auth.json"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectAuthConfig(agentDir); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("error=%v want unsupported profile", err)
	}
}

func TestInspectPolicySourcesIncludesNativePolicyFiles(t *testing.T) {
	workspace := canonicalTempDir(t)
	sources, err := inspectPolicySources(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 4 || sources[0].Path >= sources[1].Path || sources[1].Path >= sources[2].Path || sources[2].Path >= sources[3].Path {
		t.Fatalf("sources=%+v", sources)
	}
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	path, err := config.CanonicalizePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEffectivePolicyBindsModelsConfigDigest(t *testing.T) {
	request := planRequest(ModeReadOnly)
	environment := profileEnvironment{WritableRoots: []string{"/tmp"}}
	runtimeSHA256 := strings.Repeat("a", 64)
	firstSource := task.PolicySourceDigest{Path: "/agent/models.json", Kind: piModelsConfigKind, Present: true, SHA256: strings.Repeat("b", 64)}
	secondSource := firstSource
	secondSource.SHA256 = strings.Repeat("c", 64)
	first, err := effectivePolicy(request, environment, runtimeSHA256, []task.PolicySourceDigest{firstSource})
	if err != nil {
		t.Fatal(err)
	}
	second, err := effectivePolicy(request, environment, runtimeSHA256, []task.PolicySourceDigest{secondSource})
	if err != nil {
		t.Fatal(err)
	}
	if task.CompareEffectiveConfigs(first, second) || first.Digest == second.Digest {
		t.Fatal("models config digest change did not alter effective policy")
	}
}
