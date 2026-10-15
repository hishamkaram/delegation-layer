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

func TestCurrentNativeProfileAdmitsWithoutRuntimeFacts(t *testing.T) {
	request, now := profileReconstructionFixture(t)
	candidate := mustPrepareCandidate(t, request)
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
	capability, err := commonprovider.RuntimeCapabilityDigest(*candidate.Inspection)
	if err != nil || static.RuntimePreflightSHA256 != capability {
		t.Fatalf("static capability digest=%q want=%q err=%v", static.RuntimePreflightSHA256, capability, err)
	}
	if static.Plan.RuntimePreflightSHA256 != "" {
		t.Fatalf("provider static admission populated plan marker=%q", static.Plan.RuntimePreflightSHA256)
	}
	if _, err = candidate.Finalize(nil, now); err == nil {
		t.Fatal("finalizer accepted missing runtime facts")
	}

	finalized := finalizeProfile(t, candidate, now)
	if finalized.RuntimePreflightSHA256 != "" || finalized.ObservedVersion == "" {
		t.Fatalf("finalized profile retained pending runtime state: %+v", finalized)
	}
	if !slices.Equal(static.Plan.Arguments, finalized.Plan.Arguments) ||
		!slices.Equal(static.Plan.Environment, finalized.Plan.Environment) ||
		!task.CompareEffectiveConfigs(static.Effective, finalized.Effective) {
		t.Fatalf("static and finalized launch contracts differ: static=%+v finalized=%+v", static, finalized)
	}
}

func TestPendingCurrentNativeProfileReconstructsStaticAdmission(t *testing.T) {
	request, _ := profileReconstructionFixture(t)
	candidate := mustPrepareCandidate(t, request)
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
	workspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
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
