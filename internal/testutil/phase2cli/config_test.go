package phase2cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/testutil/phase2fixture"
)

func TestLoadHarnessConfigIsStrictAndBounded(t *testing.T) {
	dir := canonicalTestDir(t)
	fixture := testHarnessFixture(t, dir)
	path := filepath.Join(dir, fixtureConfigName)
	writeCanonicalTestFile(t, path, fixture.raw)
	loaded, err := loadHarnessConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.ProviderConfig != fixture.config.ProviderConfig || task.ComputeSHA256(loaded.Raw) != task.ComputeSHA256(fixture.raw) {
		t.Fatalf("loaded config changed: %+v", loaded)
	}

	bad := append([]byte(nil), fixture.raw...)
	bad = append(bad[:len(bad)-2], []byte(`,"Schema_version":1}`+"\n")...)
	writeCanonicalTestFile(t, path, bad)
	if _, err = loadHarnessConfig(path); err == nil {
		t.Fatal("wrong-case config field accepted")
	}
}

func TestValidateHarnessConfigRejectsUnboundedOrMisplacedHooks(t *testing.T) {
	dir := canonicalTestDir(t)
	fixture := testHarnessFixture(t, dir)
	fixture.config.Hooks = HookConfig{Mode: HookNormal, DelayMS: 10001, ReleasePath: filepath.Join(dir, "release")}
	if err := validateHarnessConfig(fixture.config); err == nil {
		t.Fatal("invalid normal hook accepted")
	}

	fixture.config.Hooks = HookConfig{Mode: HookNormal, DelayMS: 0, ReleasePath: filepath.Join(dir, "release")}
	if err := validateHarnessConfig(fixture.config); err != nil {
		t.Fatal(err)
	}

	fixture.config.Hooks = HookConfig{Mode: HookStartDelayed, DelayMS: 10, ReleasePath: filepath.Join(dir, "release")}
	if err := validateHarnessConfig(fixture.config); err != nil {
		t.Fatal(err)
	}
	fixture.config.Hooks.ReleasePath = dir + string(filepath.Separator) + ".." + string(filepath.Separator) + "release"
	if err := validateHarnessConfig(fixture.config); err == nil {
		t.Fatal("unclean release path accepted")
	}

	fixture.config.Hooks = HookConfig{Mode: HookPublicationDelayed, DelayMS: 0, ReleasePath: filepath.Join(dir, "publication.release")}
	if err := validateHarnessConfig(fixture.config); err != nil {
		t.Fatal(err)
	}
	fixture.config.Hooks.ReleasePath = ""
	if err := validateHarnessConfig(fixture.config); err == nil {
		t.Fatal("publication-delayed hook accepted without explicit release path")
	}
}

func TestPrepareProfileReadsAndBindsAllFixtureDigests(t *testing.T) {
	dir := canonicalTestDir(t)
	fixture := testHarnessFixture(t, dir)
	configPath := filepath.Join(dir, fixtureConfigName)
	writeCanonicalTestFile(t, configPath, fixture.raw)
	m := &Main{configPath: configPath}
	request := task.TaskRecord{TaskID: fixture.provider.TaskID, Provider: phase2fixture.Provider, Mode: phase2fixture.Mode, CanonicalCwd: dir}
	profile, err := m.prepareProfile(request)
	if err != nil {
		t.Fatal(err)
	}
	if err = profile.Validate(request); err != nil {
		t.Fatal(err)
	}
	if profile.Plan.Executable != fixture.config.ProviderExecutable || profile.Plan.Directory != dir || profile.ObservedVersion != fixtureObservedVersion {
		t.Fatalf("profile plan=%+v version=%q", profile.Plan, profile.ObservedVersion)
	}
	wantArgs := append([]string{fixture.config.ProviderConfig}, fixture.provider.Argv...)
	if !equalStringSlices(profile.Plan.Arguments, wantArgs) {
		t.Fatalf("provider argv=%q want=%q", profile.Plan.Arguments, wantArgs)
	}
	if profile.Effective.Digest != task.ComputeSHA256(fixture.raw) {
		t.Fatalf("effective digest=%q", profile.Effective.Digest)
	}
	if profile.Effective.Containment != "finite-fixture-process" || profile.Effective.Approval != "fixture-no-tools" {
		t.Fatalf("effective=%+v", profile.Effective)
	}
	fixture.config.Environment = []string{}
	fixture.raw, err = task.MarshalCanonical(fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	writeCanonicalTestFile(t, configPath, fixture.raw)
	profile, err = m.prepareProfile(request)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Plan.Environment == nil {
		t.Fatal("empty explicit environment became ambient environment")
	}

	request.RequestedConfig.Model = "forbidden-model"
	if _, err = m.prepareProfile(request); err == nil {
		t.Fatal("model accepted by fixture profile")
	}
	request.RequestedConfig.Model = ""
	request.TaskID = "ffffffffffffffffffffffffffffffff"
	if _, err = m.prepareProfile(request); !errors.Is(err, task.ErrIdentityMismatch) {
		t.Fatalf("provider task mismatch=%v", err)
	}
}

func TestPrepareProfileRejectsNativeTimeoutBeforeReadingConfig(t *testing.T) {
	m := &Main{configPath: filepath.Join(t.TempDir(), "missing-config.json")}
	request := task.TaskRecord{
		Provider:        phase2fixture.Provider,
		Mode:            phase2fixture.Mode,
		RequestedConfig: task.TaskConfig{Permission: phase2fixture.Mode, NativeTimeout: "1s"},
	}
	if _, err := m.prepareProfile(request); err == nil || !strings.Contains(err.Error(), "native timeout") {
		t.Fatalf("native timeout was not rejected before config access: %v", err)
	}
}

func TestNewMainKeepsRecoveryIndependentFromMissingConfig(t *testing.T) {
	m := NewMain(filepath.Join(t.TempDir(), "missing-delegate"))
	if !filepath.IsAbs(m.Dependencies().InitialSupervisorExecutable) {
		t.Fatalf("initial supervisor path=%q", m.Dependencies().InitialSupervisorExecutable)
	}
	if _, err := m.Dependencies().PredicateRegistry().Resolve(phase2fixture.Predicate().Reference()); err != nil {
		t.Fatal(err)
	}
	request := task.TaskRecord{TaskID: "0123456789abcdef0123456789abcdef", Provider: phase2fixture.Provider, Mode: phase2fixture.Mode}
	if _, err := m.Dependencies().PrepareProfile(request); err == nil {
		t.Fatal("missing sibling config unexpectedly prepared a profile")
	}
}

type harnessFixture struct {
	config   HarnessConfig
	provider phase2fixture.ProviderConfig
	raw      []byte
}

func testHarnessFixture(t *testing.T, dir string) harnessFixture {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	providerConfig := phase2fixture.ProviderConfig{Scenario: "success", ArtifactDir: filepath.Join(dir, "artifacts"), LifetimeMS: 100, TaskID: "0123456789abcdef0123456789abcdef", SessionID: "conv-1", Argv: []string{"$(literal)", "value;token"}}
	providerData, err := task.MarshalCanonical(providerConfig)
	if err != nil {
		t.Fatal(err)
	}
	providerPath := filepath.Join(dir, "provider.json")
	writeCanonicalTestFile(t, providerPath, providerData)
	events := filepath.Join(dir, "events")
	config := HarnessConfig{
		SchemaVersion:        1,
		ProviderExecutable:   executable,
		ProviderSHA256:       mustHashRegular(t, executable),
		ProviderConfig:       providerPath,
		ProviderConfigSHA256: task.ComputeSHA256(providerData),
		SupervisorExecutable: filepath.Join(dir, "missing-supervisor"),
		EventsDirectory:      events,
		Environment:          []string{"FIXTURE_LITERAL=$(no-shell)", "PATH=/literal"},
		Hooks:                HookConfig{Mode: HookNormal, DelayMS: 0},
	}
	raw, err := task.MarshalCanonical(config)
	if err != nil {
		t.Fatal(err)
	}
	return harnessFixture{config: config, provider: providerConfig, raw: raw}
}

func canonicalTestDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustHashRegular(t *testing.T, path string) string {
	t.Helper()
	digest, err := hashRegular(path)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func writeCanonicalTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
