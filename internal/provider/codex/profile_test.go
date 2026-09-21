package codex

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func TestProfileReconstructionRetainsNativeAndLegacyBindings(t *testing.T) {
	t.Run("native", testNativeProfileReconstruction)
	t.Run("pre-native-runtime-reported", func(t *testing.T) {
		t.Helper()
		testHistoricalProfileReconstruction(t, Reference())
	})
	t.Run("legacy", func(t *testing.T) {
		t.Helper()
		testHistoricalProfileReconstruction(t, LegacyReference())
	})
}

func testNativeProfileReconstruction(t *testing.T) {
	t.Helper()
	request, now := profileReconstructionFixture(t)
	nativeProfile := finalizeProfile(t, mustPrepareCandidate(t, request), now)
	requireNativeProfile(t, nativeProfile)

	nativeMeta := profileMeta(nativeProfile)
	nativeReplayCandidate, err := PrepareExistingCandidate(request, nativeMeta)
	if err != nil {
		t.Fatal(err)
	}
	nativeReplay := finalizeProfile(t, nativeReplayCandidate, now)
	if !task.CompareEffectiveConfigs(nativeProfile.Effective, nativeReplay.Effective) {
		t.Fatalf("native reconstruction changed effective config: replay=%+v", nativeReplay.Effective)
	}
	if err := nativeReplay.Matches(request, nativeMeta); err != nil {
		t.Fatalf("native metadata no longer matches: %v", err)
	}
}

func testHistoricalProfileReconstruction(t *testing.T, storedPredicate task.PredicateRef) {
	t.Helper()
	request, now := profileReconstructionFixture(t)
	legacyCandidate, err := PrepareExistingCandidate(request, task.MetaRecord{EffectiveConfig: task.EffectiveConfig{
		Policy: &task.PolicyDetails{ProfileRevision: ProfileRevision},
	}, Predicate: storedPredicate})
	if err != nil {
		t.Fatal(err)
	}
	requireLegacyInspection(t, legacyCandidate)

	legacyArguments, output, err := legacyExecArguments(request)
	if err != nil {
		t.Fatal(err)
	}
	legacyProfile := finalizeProfile(t, legacyCandidate, now)
	if !slices.Equal(legacyProfile.Plan.Arguments, legacyArguments) || legacyProfile.Plan.OutputArtifacts[0].ArgumentIndex != output || !legacyProfile.Plan.Predicate.Equal(storedPredicate) {
		t.Fatalf("legacy launch reconstruction args=%q output=%+v predicate=%+v", legacyProfile.Plan.Arguments, legacyProfile.Plan.OutputArtifacts, legacyProfile.Plan.Predicate)
	}
	if err := legacyProfile.Matches(request, profileMeta(legacyProfile)); err != nil {
		t.Fatalf("historical metadata no longer matches: %v", err)
	}
}

func profileReconstructionFixture(t *testing.T) (task.TaskRecord, time.Time) {
	t.Helper()
	home := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cliDir := t.TempDir()
	cliPath := filepath.Join(cliDir, "codex")
	if err := os.WriteFile(cliPath, []byte("codex fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", cliDir)
	codexHome := filepath.Join(home, ".codex")
	t.Setenv("CODEX_HOME", codexHome)
	now := time.Now()
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), marshalAuth(t, fixtureAuth(t, now, "pro")), 0o600); err != nil {
		t.Fatal(err)
	}

	request := profileRequest()
	request.CanonicalCwd = workspace
	return request, now
}

func mustPrepareCandidate(t *testing.T, request task.TaskRecord) commonprovider.ProfileCandidate {
	t.Helper()
	candidate, err := PrepareCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}

func finalizeProfile(t *testing.T, candidate commonprovider.ProfileCandidate, now time.Time) commonprovider.PreparedProfile {
	t.Helper()
	if candidate.Inspection == nil {
		t.Fatal("profile candidate has no runtime inspection")
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{
		Executable: candidate.Inspection.Executable,
		Version:    "fixture",
		SHA256:     candidate.Inspection.ExecutableSHA256,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := candidate.Finalize(facts, now)
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func profileMeta(profile commonprovider.PreparedProfile) task.MetaRecord {
	return task.MetaRecord{
		EffectiveConfig:      task.CloneEffectiveConfig(profile.Effective),
		ProviderExecutable:   profile.Plan.Executable,
		ProviderVersion:      profile.ObservedVersion,
		Environment:          slices.Clone(profile.Plan.Environment),
		Predicate:            profile.Plan.Predicate,
		OutputArtifacts:      slices.Clone(profile.Plan.OutputArtifacts),
		OutputWriterContract: profile.Plan.OutputWriterContract,
	}
}

func requireNativeProfile(t *testing.T, profile commonprovider.PreparedProfile) {
	t.Helper()
	policy := profile.Effective.Policy
	if policy == nil || policy.ProfileRevision != commonprovider.NativeProfileRevision || len(policy.Sources) != 0 {
		t.Fatalf("native effective policy=%+v", policy)
	}
	if !profile.Plan.Predicate.Equal(Reference()) {
		t.Fatalf("native predicate=%+v", profile.Plan.Predicate)
	}
}

func requireLegacyInspection(t *testing.T, candidate commonprovider.ProfileCandidate) {
	t.Helper()
	if candidate.Inspection == nil || candidate.Inspection.Runtime == nil {
		t.Fatalf("legacy inspection=%+v", candidate.Inspection)
	}
	legacyRequirements := legacyRuntimeRequirements()
	if !slices.Equal(candidate.Inspection.Runtime.RequiredFlags, legacyRequirements.RequiredFlags) {
		t.Fatalf("legacy inspection requirements=%+v", candidate.Inspection)
	}
}
