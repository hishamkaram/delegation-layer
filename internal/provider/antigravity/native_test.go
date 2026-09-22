package antigravity

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestNativePreparationLeavesProviderConfigurationAlone(t *testing.T) {
	home, workspace := profileFixture(t)
	executable := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(executable, []byte("fixture executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Dir(executable))
	t.Setenv("XDG_DATA_DIRS", "/usr/local/share:/usr/share")
	t.Setenv("GEMINI_API_KEY", "fixture-secret")
	configPath := filepath.Join(home, ".gemini", "config", "mcp_config.json")
	content := []byte(`{"mcpServers":{"example":{"command":"example"}}}`)
	writeSourceTestFile(t, configPath, content)
	request := argumentRequest()
	request.CanonicalCwd = workspace
	candidate, err := PrepareCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{Executable: candidate.Inspection.Executable, SHA256: candidate.Inspection.ExecutableSHA256, Version: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := candidate.Finalize(facts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if profile.Effective.Policy.ProfileRevision != commonprovider.NativeProfileRevision || len(profile.Effective.Policy.Sources) != 0 {
		t.Fatal("native configuration was inventoried")
	}
	if slices.Contains(profile.Plan.Arguments, "--disable-slash-commands") {
		t.Fatal("native customization disabled")
	}
	assertUnattendedNativeApproval(t, profile, candidate.Inspection.Runtime.RequiredFlags)
	if !slices.Contains(profile.Plan.Environment, "XDG_DATA_DIRS=/usr/local/share:/usr/share") || slices.Contains(profile.Plan.Environment, "GEMINI_API_KEY=fixture-secret") {
		t.Fatal("incorrect discovery or credential environment")
	}
	actual, err := os.ReadFile(configPath)
	if err != nil || string(actual) != string(content) {
		t.Fatal("provider configuration changed")
	}
	assertNativeReconstruction(t, request, profile, facts)
}

func TestNativeReadOnlyUsesPlanWithoutBypass(t *testing.T) {
	request, candidate := prepareReadOnlyNativeCandidate(t)
	facts := readOnlyRuntimeFacts(t, candidate)
	assertReadOnlyProbe(t, candidate)
	profile := finalizeReadOnlyProfile(t, candidate, facts)
	assertReadOnlyProfile(t, profile)
	assertReadOnlyReconstruction(t, request, profile, facts)
}

func prepareReadOnlyNativeCandidate(t *testing.T) (task.TaskRecord, commonprovider.ProfileCandidate) {
	t.Helper()
	home, workspace := profileFixture(t)
	executable := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(executable, []byte("fixture executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Dir(executable))
	request := argumentRequest()
	request.Mode = ModeReadOnly
	request.RequestedConfig.Permission = ModeReadOnly
	request.CanonicalCwd = workspace
	candidate, err := PrepareCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	return request, candidate
}

func readOnlyRuntimeFacts(t *testing.T, candidate commonprovider.ProfileCandidate) []byte {
	t.Helper()
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{Executable: candidate.Inspection.Executable, SHA256: candidate.Inspection.ExecutableSHA256, Version: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

func assertReadOnlyProbe(t *testing.T, candidate commonprovider.ProfileCandidate) {
	t.Helper()
	if slices.Contains(candidate.Inspection.Runtime.RequiredFlags, "--dangerously-skip-permissions") {
		t.Fatal("read-only runtime probe retained bypass capability")
	}
}

func finalizeReadOnlyProfile(t *testing.T, candidate commonprovider.ProfileCandidate, facts []byte) commonprovider.PreparedProfile {
	t.Helper()
	profile, err := candidate.Finalize(facts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func assertReadOnlyProfile(t *testing.T, profile commonprovider.PreparedProfile) {
	t.Helper()
	if !slices.Contains(profile.Plan.Arguments, "--sandbox") {
		t.Fatal("read-only native argv omitted sandbox")
	}
	if !slices.Contains(profile.Plan.Arguments, "plan") {
		t.Fatal("read-only native argv omitted plan mode")
	}
	if slices.Contains(profile.Plan.Arguments, "--dangerously-skip-permissions") {
		t.Fatalf("read-only native argv=%q", profile.Plan.Arguments)
	}
	if profile.Effective.Containment != ModeReadOnly {
		t.Fatalf("read-only containment=%q", profile.Effective.Containment)
	}
	if profile.Effective.Approval != "plan" {
		t.Fatalf("read-only approval=%q", profile.Effective.Approval)
	}
	if !profile.Plan.Predicate.Equal(NewCurrentPrintInterpreterForMode(ModeReadOnly).Reference()) {
		t.Fatalf("read-only predicate=%+v", profile.Plan.Predicate)
	}
}

func assertReadOnlyReconstruction(t *testing.T, request task.TaskRecord, profile commonprovider.PreparedProfile, facts []byte) {
	t.Helper()
	reconstructed, err := PrepareExistingCandidate(request, task.MetaRecord{EffectiveConfig: profile.Effective})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := reconstructed.Finalize(facts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !task.CompareEffectiveConfigs(profile.Effective, replay.Effective) {
		t.Fatal("read-only effective configuration changed during reconstruction")
	}
	if !slices.Equal(profile.Plan.Arguments, replay.Plan.Arguments) {
		t.Fatal("read-only arguments changed during reconstruction")
	}
	if !profile.Plan.Predicate.Equal(replay.Plan.Predicate) {
		t.Fatal("read-only predicate changed during reconstruction")
	}
}

func assertUnattendedNativeApproval(t *testing.T, profile commonprovider.PreparedProfile, requiredFlags []string) {
	t.Helper()
	if !slices.Contains(profile.Plan.Arguments, "--dangerously-skip-permissions") ||
		!slices.Contains(profile.Plan.Arguments, "--sandbox") || profile.Effective.Approval != unattendedApproval ||
		!slices.Contains(requiredFlags, "--dangerously-skip-permissions") {
		t.Fatal("unattended native execution was not requested and recorded")
	}
}

func assertNativeReconstruction(t *testing.T, request task.TaskRecord, profile commonprovider.PreparedProfile, facts []byte) {
	t.Helper()
	reconstructed, err := PrepareExistingCandidate(request, task.MetaRecord{EffectiveConfig: profile.Effective})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := reconstructed.Finalize(facts, time.Now())
	if err != nil || !task.CompareEffectiveConfigs(profile.Effective, replay.Effective) || !slices.Equal(profile.Plan.Arguments, replay.Plan.Arguments) {
		t.Fatal("native profile reconstruction changed")
	}
	historical, err := commonprovider.NativeEffectiveConfig(request, profile.WritableRoots, profile.Effective.Policy.RuntimeSHA256, "accept-edits")
	if err != nil {
		t.Fatal(err)
	}
	oldCandidate, err := PrepareExistingCandidate(request, task.MetaRecord{EffectiveConfig: historical, Environment: profile.Plan.Environment})
	if err != nil {
		t.Fatal(err)
	}
	oldProfile, err := oldCandidate.Finalize(facts, time.Now())
	if err != nil || !task.CompareEffectiveConfigs(historical, oldProfile.Effective) ||
		slices.Contains(oldProfile.Plan.Arguments, "--dangerously-skip-permissions") ||
		slices.Contains(oldCandidate.Inspection.Runtime.RequiredFlags, "--dangerously-skip-permissions") {
		t.Fatal("historical native task approval was escalated")
	}
	wantOldArguments := slices.DeleteFunc(slices.Clone(profile.Plan.Arguments), func(argument string) bool { return argument == "--dangerously-skip-permissions" })
	if !slices.Equal(oldProfile.Plan.Arguments, wantOldArguments) {
		t.Fatal("historical native launch arguments changed")
	}
	if _, err = PrepareExistingCandidate(request, task.MetaRecord{}); err == nil {
		t.Fatal("historical configuration restrictions silently relaxed")
	}
}

func TestNativeEnvironmentReconstructsRuntimePaths(t *testing.T) {
	home := t.TempDir()
	initial, err := prepareProfileEnvironment([]string{"HOME=" + home, "GOPATH=" + filepath.Join(home, "custom-go"), "GOCACHE=" + filepath.Join(home, "custom-cache"), "XDG_DATA_DIRS=/usr/share", "XDG_DATA_HOME=" + filepath.Join(home, "native-data")}, true)
	if err != nil {
		t.Fatal(err)
	}
	reconstructed, err := prepareProfileEnvironment(initial.Values, true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(initial.WritableRoots, reconstructed.WritableRoots) || !slices.Equal(initial.Values, reconstructed.Values) {
		t.Fatal("native runtime paths drifted after environment reconstruction")
	}
}

func TestNativeEnvironmentReservesDiscoveryRootsAndRejectsRelativePaths(t *testing.T) {
	home := t.TempDir()
	configHome := filepath.Join(home, "config")
	configA := filepath.Join(home, "config-a")
	configB := filepath.Join(home, "config-b")
	dataHome := filepath.Join(home, "data")
	dataA := filepath.Join(home, "data-a")
	dataB := filepath.Join(home, "data-b")
	stateHome := filepath.Join(home, "state")
	geminiHome := filepath.Join(home, "gemini")
	agyHome := filepath.Join(home, "agy")
	values := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + configHome,
		"XDG_CONFIG_DIRS=" + configA + string(filepath.ListSeparator) + configB,
		"XDG_DATA_HOME=" + dataHome,
		"XDG_DATA_DIRS=" + dataA + string(filepath.ListSeparator) + dataB,
		"XDG_STATE_HOME=" + stateHome,
		"GEMINI_HOME=" + geminiHome,
		"AGY_HOME=" + agyHome,
	}
	environment, err := prepareProfileEnvironment(values, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{configHome, configA, configB, dataHome, dataA, dataB, stateHome, geminiHome, agyHome} {
		canonical, pathErr := config.CanonicalizePath(root)
		if pathErr != nil || !slices.Contains(environment.WritableRoots, canonical) {
			t.Fatalf("discovery root %q was not reserved: %q", root, environment.WritableRoots)
		}
	}
	for _, entry := range []string{"XDG_CONFIG_HOME=relative", "XDG_DATA_DIRS=relative:/absolute", "GEMINI_HOME=relative"} {
		if _, err := prepareProfileEnvironment([]string{"HOME=" + home, entry}, true); err == nil {
			t.Fatalf("relative discovery path accepted: %s", entry)
		}
	}
}

func TestNativeReconstructionRetainsRecordedWritableRoots(t *testing.T) {
	home, workspace := profileFixture(t)
	executable := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(executable, []byte("fixture executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	values := []string{
		"HOME=" + home,
		"PATH=" + filepath.Dir(executable),
		"XDG_DATA_DIRS=" + filepath.Join(home, "new-discovery-root"),
		"XDG_CONFIG_HOME=relative",
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", filepath.Dir(executable))
	request := argumentRequest()
	request.CanonicalCwd = workspace
	candidate, err := PrepareExistingCandidate(request, task.MetaRecord{
		Environment: values,
		EffectiveConfig: task.EffectiveConfig{Approval: "accept-edits", Policy: &task.PolicyDetails{
			ProfileRevision: commonprovider.NativeProfileRevision,
			WritableRoots:   []string{"/tmp", filepath.Join(home, ".gemini")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/tmp", filepath.Join(home, ".gemini")}
	if !slices.Equal(candidate.WritableRoots, want) {
		t.Fatalf("reconstruction changed recorded roots: got=%q want=%q", candidate.WritableRoots, want)
	}
	if slices.Contains(candidate.WritableRoots, filepath.Join(home, "new-discovery-root")) {
		t.Fatal("current discovery root was added to an admitted task")
	}
}
