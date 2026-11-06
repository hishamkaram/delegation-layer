package harnesscli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/testutil/fakeprovider"
)

func TestPrepareProfileSelectsExecutableMetadataWithoutContentFingerprint(t *testing.T) {
	fixture := newInstalledProfileFixture(t)
	main, request, executable, raw := fixture.main, fixture.request, fixture.executable, fixture.raw
	profile, err := main.prepareProfile(request)
	if err != nil {
		t.Fatalf("installed metadata fixture was rejected: %v", err)
	}
	if profile.Plan.Executable != executable || profile.Plan.ExecutableSHA256 != "" || profile.Effective.Digest != task.ComputeSHA256(raw) {
		t.Fatalf("fixture lost installed selection or config data binding: %+v", profile)
	}
	if err = os.Chmod(executable, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(executable, []byte("#!/bin/sh\n# installed update\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(executable, 0o111); err != nil {
		t.Fatal(err)
	}
	updated, err := main.prepareProfile(request)
	if err != nil || updated.Plan.Executable != executable || updated.Plan.ExecutableSHA256 != "" || updated.Effective.Digest != profile.Effective.Digest {
		t.Fatalf("same selected installation gained a byte identity gate: %v", err)
	}
	retired := bytes.TrimSpace(raw)
	retired = append(append([]byte(nil), retired[:len(retired)-1]...), []byte(",\"provider_sha256\":null}\n")...)
	writeCanonicalTestFile(t, fixture.main.configPath, retired)
	if _, configErr := loadHarnessConfig(fixture.main.configPath); configErr == nil {
		t.Fatal("retired program SHA fixture field was accepted")
	}
	writeCanonicalTestFile(t, fixture.main.configPath, raw)
	if err = os.Chmod(executable, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := main.prepareProfile(request); err == nil {
		t.Fatal("non-executable provider was accepted")
	}
}

type installedProfileFixture struct {
	main       *Main
	request    task.TaskRecord
	executable string
	raw        []byte
}

func newInstalledProfileFixture(t *testing.T) installedProfileFixture {
	t.Helper()
	dir := canonicalTestDir(t)
	executable := filepath.Join(dir, "provider")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o111); err != nil {
		t.Fatal(err)
	}
	provider := fakeprovider.ProviderConfig{Scenario: "success", ArtifactDir: filepath.Join(dir, "artifacts"), LifetimeMS: 100, TaskID: "0123456789abcdef0123456789abcdef", SessionID: "session-1", Argv: []string{}}
	providerData, err := task.MarshalCanonical(provider)
	if err != nil {
		t.Fatal(err)
	}
	providerPath := filepath.Join(dir, "provider.json")
	writeCanonicalTestFile(t, providerPath, providerData)
	// This is the fresh fixture schema: no historical program SHA property.
	cfg := struct {
		SchemaVersion        int        `json:"schema_version"`
		ProviderExecutable   string     `json:"provider_executable"`
		ProviderConfig       string     `json:"provider_config"`
		ProviderConfigSHA256 string     `json:"provider_config_sha256"`
		SupervisorExecutable string     `json:"supervisor_executable"`
		EventsDirectory      string     `json:"events_directory"`
		Environment          []string   `json:"environment"`
		Hooks                HookConfig `json:"hooks"`
	}{1, executable, providerPath, task.ComputeSHA256(providerData), filepath.Join(dir, "pueue"), filepath.Join(dir, "events"), []string{}, HookConfig{Mode: HookNormal}}
	raw, err := task.MarshalCanonical(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, fixtureConfigName)
	writeCanonicalTestFile(t, configPath, raw)
	main := &Main{configPath: configPath}
	request := task.TaskRecord{TaskID: provider.TaskID, Provider: fakeprovider.Provider, Mode: fakeprovider.Mode, CanonicalCwd: dir}
	return installedProfileFixture{main: main, request: request, executable: executable, raw: raw}
}
