package antigravity

import (
	"encoding/json"
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

func TestEffectivePolicyTracksSourceAndMembershipDrift(t *testing.T) {
	home, workspace := profileFixture(t)
	first := resolveFixturePolicy(t, home, workspace)
	if first.Policy == nil || first.Policy.ProfileRevision != ProfileRevision || len(first.Policy.Sources) == 0 ||
		first.Approval != "accept-edits:request-review:headless-deny" {
		t.Fatal("normalized policy decisions were not retained")
	}
	if !task.CompareEffectiveConfigs(first, resolveFixturePolicy(t, home, workspace)) {
		t.Fatal("unchanged policy produced a different snapshot")
	}
	writeFixtureSettings(t, home, workspace, "changed-model")
	second := resolveFixturePolicy(t, home, workspace)
	if task.CompareEffectiveConfigs(first, second) || first.Digest == second.Digest {
		t.Fatal("source change was not bound into effective policy")
	}
	if err := os.Mkdir(filepath.Join(workspace, ".agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	third := resolveFixturePolicy(t, home, workspace)
	if task.CompareEffectiveConfigs(second, third) || second.Digest == third.Digest {
		t.Fatal("directory creation was not bound into effective policy")
	}
}

func TestEffectivePolicyRejectsProjectOverridesAndUntrustedWorkspace(t *testing.T) {
	home, workspace := profileFixture(t)
	project := filepath.Join(home, ".gemini", "config", "projects", "default-cli-project.json")
	writeSourceTestFile(t, project, []byte(`{"id":"default-cli-project","name":"CLI Project","projectResources":{},"allowNonWorkspaceAccess":true}`))
	inventory, err := InventorySources(InventoryRequest{Home: home, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	environment := fixtureProfileEnvironment(t, home)
	if _, err = resolveEffectivePolicy(fixtureRuntime(), environment, inventory); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("project override accepted: %v", err)
	}
	if err = validateWorkspaceTrust([]string{home}, workspace); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("unrelated trust root accepted: %v", err)
	}
	if err = validateWorkspaceTrust([]string{filepath.Dir(workspace)}, workspace); err != nil {
		t.Fatalf("canonical ancestor candidate refused before native verification: %v", err)
	}
}

func TestEnvironmentUsesNativeLoginWithoutPersistingAmbientCredentials(t *testing.T) {
	home, workspace := profileFixture(t)
	const secret = "fixture-secret-not-to-be-persisted"
	values := []string{"HOME=" + home, "PATH=/usr/bin", "GEMINI_API_KEY=" + secret}
	environment, err := prepareEnvironment(values)
	if err != nil {
		t.Fatal(err)
	}
	values[2] = "GEMINI_API_KEY=changed"
	if slices.Contains(environment.Values, "GEMINI_API_KEY="+secret) || slices.Contains(environment.Values, "GEMINI_API_KEY=changed") {
		t.Fatal("ambient credential entered the provider runtime environment")
	}
	if !slices.Contains(environment.Values, "HOME="+home) || !slices.Contains(environment.Values, "PATH=/usr/bin") {
		t.Fatalf("native login environment was not retained: %q", environment.Values)
	}
	inventory, err := InventorySources(InventoryRequest{Home: home, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	effective, err := resolveEffectivePolicy(fixtureRuntime(), environment, inventory)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := task.MarshalCanonical(effective)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "GEMINI_API_KEY") {
		t.Fatal("credential environment entered persisted policy")
	}
}

func TestEnvironmentDropsSupervisorBookkeepingAndSortsValues(t *testing.T) {
	home, _ := profileFixture(t)
	environment, err := prepareEnvironment([]string{
		"SHLVL=2", "PUEUE_GROUP=default", "HOME=" + home,
		"GEMINI_API_KEY=fixture-secret", "OLDPWD=" + home, "_=delegate",
		"PUEUE_WORKER_ID=0", "PATH=/usr/bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"SHLVL", "PUEUE_GROUP", "PUEUE_WORKER_ID", "OLDPWD", "_"} {
		for _, entry := range environment.Values {
			if strings.HasPrefix(entry, key+"=") {
				t.Fatalf("volatile supervisor value %q was retained", entry)
			}
		}
	}
	if !slices.IsSorted(environment.Values) {
		t.Fatalf("environment values are not deterministic: %q", environment.Values)
	}
	if slices.Contains(environment.Values, "GEMINI_API_KEY=fixture-secret") {
		t.Fatal("ambient credential entered the queued task environment")
	}
}

func TestEnvironmentRejectsDiscoveryOverridesAndMalformedRoots(t *testing.T) {
	home, _ := profileFixture(t)
	for _, extra := range []string{"AGY_HOME=/other", "XDG_CONFIG_HOME=/other", "TMPDIR=relative", "GOCACHE=relative", "GOCACHE=OFF", "GOCACHE=off/", "HOME=/duplicate", "invalid"} {
		if _, err := prepareEnvironment([]string{"HOME=" + home, extra}); !errors.Is(err, ErrUnsupportedProfile) {
			t.Errorf("unsupported environment %q accepted: %v", extra, err)
		}
	}
}

func TestEnvironmentAcceptsDisabledGoCacheSentinel(t *testing.T) {
	home, _ := profileFixture(t)
	environment, err := prepareEnvironment([]string{"HOME=" + home, "GOCACHE=off"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(environment.WritableRoots, "off") {
		t.Fatal("GOCACHE=off was recorded as a writable root")
	}
}

func TestEffectivePolicyNormalizesExplicitEmptyToolPermission(t *testing.T) {
	home, workspace := profileFixture(t)
	settings, err := json.Marshal(map[string]any{
		"model":             "initial-model",
		"trustedWorkspaces": []string{workspace},
		"toolPermission":    "",
	})
	if err != nil {
		t.Fatal(err)
	}
	writeSourceTestFile(t, filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"), settings)
	effective := resolveFixturePolicy(t, home, workspace)
	if effective.Approval != "accept-edits:request-review:headless-deny" {
		t.Fatalf("effective approval = %q", effective.Approval)
	}
}

func TestCurrentNativeProfileAdmitsWithoutRuntimeFacts(t *testing.T) {
	home, workspace := profileFixture(t)
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "agy"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", binDir)
	request := argumentRequest()
	request.CanonicalCwd = workspace
	candidate, err := PrepareCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	static := requireStaticAdmission(t, candidate)
	assertFinalizedAdmission(t, candidate, static)
}

func requireStaticAdmission(t *testing.T, candidate commonprovider.ProfileCandidate) commonprovider.PreparedProfile {
	t.Helper()
	if candidate.Admit == nil {
		t.Fatal("current native candidate does not expose static admission")
	}
	static, err := candidate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	if static.ObservedVersion != "" || static.Plan.ProviderRuntime != (task.ProviderRuntimeIdentity{}) {
		t.Fatalf("static admission claimed runtime facts: version=%q runtime=%+v", static.ObservedVersion, static.Plan.ProviderRuntime)
	}
	err = task.ValidateSHA256(static.RuntimePreflightSHA256)
	if err != nil {
		t.Fatalf("static capability digest=%q: %v", static.RuntimePreflightSHA256, err)
	}
	if candidate.Inspection == nil {
		t.Fatal("current native candidate omitted runtime inspection")
	}
	capability, err := commonprovider.RuntimeCapabilityDigest(*candidate.Inspection)
	if err != nil {
		t.Fatal(err)
	}
	if static.RuntimePreflightSHA256 != capability {
		t.Fatalf("static capability digest=%q want=%q", static.RuntimePreflightSHA256, capability)
	}
	if static.Plan.RuntimePreflightSHA256 != "" {
		t.Fatalf("provider static admission populated plan marker=%q", static.Plan.RuntimePreflightSHA256)
	}
	return static
}

func assertFinalizedAdmission(t *testing.T, candidate commonprovider.ProfileCandidate, static commonprovider.PreparedProfile) {
	t.Helper()
	if _, err := candidate.Finalize(nil, time.Now()); err == nil {
		t.Fatal("finalizer accepted missing runtime facts")
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{
		Executable: candidate.Inspection.Executable, Version: "AGY fixture", SHA256: candidate.Inspection.ExecutableSHA256,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	finalized, err := candidate.Finalize(facts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if finalized.RuntimePreflightSHA256 != "" || finalized.ObservedVersion == "" {
		t.Fatalf("finalized profile retained pending runtime state: %+v", finalized)
	}
	if !slices.Equal(static.Plan.Arguments, finalized.Plan.Arguments) ||
		!slices.Equal(static.Plan.Environment, finalized.Plan.Environment) ||
		!task.CompareEffectiveConfigs(static.Effective, finalized.Effective) ||
		!static.Plan.Predicate.Equal(finalized.Plan.Predicate) {
		t.Fatalf("static and finalized launch contracts differ: static=%+v finalized=%+v", static, finalized)
	}
}

func TestPendingCurrentNativeProfileReconstructsStaticAdmission(t *testing.T) {
	home, workspace := profileFixture(t)
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "agy"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", binDir)
	request := argumentRequest()
	request.CanonicalCwd = workspace
	candidate, err := PrepareCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := candidate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	pending, err := PrepareExistingCandidate(request, task.MetaRecord{
		EffectiveConfig:        admitted.Effective,
		RuntimePreflightSHA256: admitted.RuntimePreflightSHA256,
		Environment:            slices.Clone(admitted.Plan.Environment),
		EnvironmentRecorded:    true,
		Predicate:              admitted.Plan.Predicate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Admit == nil {
		t.Fatal("pending current native profile lost static admission")
	}
	rebuilt, err := pending.Admit()
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.ObservedVersion != "" || rebuilt.RuntimePreflightSHA256 != admitted.RuntimePreflightSHA256 {
		t.Fatalf("pending reconstruction claimed unexpected runtime state: rebuilt=%+v admitted=%+v", rebuilt, admitted)
	}
	if !slices.Equal(rebuilt.Plan.Arguments, admitted.Plan.Arguments) ||
		!slices.Equal(rebuilt.Plan.Environment, admitted.Plan.Environment) ||
		!task.CompareEffectiveConfigs(rebuilt.Effective, admitted.Effective) {
		t.Fatalf("pending reconstruction changed static launch contract: rebuilt=%+v admitted=%+v", rebuilt, admitted)
	}
}

func profileFixture(t *testing.T) (string, string) {
	t.Helper()
	base := sourceTestRoot(t)
	home, workspace := filepath.Join(base, "home"), filepath.Join(base, "workspace")
	for _, path := range []string{workspace, filepath.Join(home, ".gemini", "config", "projects"), filepath.Join(home, ".gemini", "antigravity-cli", "cache")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFixtureSettings(t, home, workspace, "initial-model")
	writeSourceTestFile(t, filepath.Join(home, ".gemini", "config", "config.json"), []byte(`{"userSettings":{"remoteControlHostname":"fixture"}}`))
	writeSourceTestFile(t, filepath.Join(home, ".gemini", "config", "projects", "default-cli-project.json"), []byte(`{"id":"default-cli-project","name":"CLI Project","projectResources":{}}`))
	writeSourceTestFile(t, filepath.Join(home, ".gemini", "antigravity-cli", "cache", "default_project_id.txt"), []byte(defaultProjectID))
	return home, workspace
}

func writeFixtureSettings(t *testing.T, home, workspace, model string) {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"model": model, "trustedWorkspaces": []string{workspace}})
	if err != nil {
		t.Fatal(err)
	}
	writeSourceTestFile(t, filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"), encoded)
}

func fixtureRuntime() runtimeIdentity {
	return runtimeIdentity{Executable: "/fixture/agy", Version: Version, SHA256: task.ComputeSHA256([]byte("fixture runtime"))}
}

func fixtureProfileEnvironment(t *testing.T, home string) profileEnvironment {
	t.Helper()
	environment, err := prepareEnvironment([]string{"HOME=" + home})
	if err != nil {
		t.Fatal(err)
	}
	return environment
}

func resolveFixturePolicy(t *testing.T, home, workspace string) task.EffectiveConfig {
	t.Helper()
	inventory, err := InventorySources(InventoryRequest{Home: home, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	effective, err := resolveEffectivePolicy(fixtureRuntime(), fixtureProfileEnvironment(t, home), inventory)
	if err != nil {
		t.Fatal(err)
	}
	return effective
}
