package pi

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
	t.Run("legacy", testLegacyProfileReconstruction)
}

func testNativeProfileReconstruction(t *testing.T) {
	t.Helper()
	request, now := profileReconstructionFixture(t)
	nativeProfile := finalizeProfile(t, mustPrepareCandidate(t, request), now)
	requireNativeProfile(t, nativeProfile)

	nativeReplayCandidate, err := PrepareExistingCandidate(request, task.MetaRecord{EffectiveConfig: nativeProfile.Effective})
	if err != nil {
		t.Fatal(err)
	}
	nativeReplay := finalizeProfile(t, nativeReplayCandidate, now)
	if !task.CompareEffectiveConfigs(nativeProfile.Effective, nativeReplay.Effective) {
		t.Fatalf("native reconstruction changed effective config: replay=%+v", nativeReplay.Effective)
	}
}

func testLegacyProfileReconstruction(t *testing.T) {
	t.Helper()
	request, now := profileReconstructionFixture(t)
	legacyCandidate, err := PrepareExistingCandidate(request, task.MetaRecord{EffectiveConfig: task.EffectiveConfig{
		Policy: &task.PolicyDetails{ProfileRevision: ProfileRevision},
	}})
	if err != nil {
		t.Fatal(err)
	}
	requireLegacyInspection(t, legacyCandidate)

	legacyProfile := finalizeProfile(t, legacyCandidate, now)
	legacyArguments, err := legacyJSONArguments(request)
	if err != nil {
		t.Fatal(err)
	}
	legacyEnvironment, err := prepareProfileEnvironment(os.Environ(), false)
	if err != nil {
		t.Fatal(err)
	}
	legacyArguments = append(legacyArguments, "--session-dir", legacyEnvironment.SessionDir)
	if !slices.Equal(legacyProfile.Plan.Arguments, legacyArguments) || !legacyProfile.Plan.Predicate.Equal(LegacyReference()) {
		t.Fatalf("legacy launch reconstruction args=%q predicate=%+v", legacyProfile.Plan.Arguments, legacyProfile.Plan.Predicate)
	}
}

func profileReconstructionFixture(t *testing.T) (task.TaskRecord, time.Time) {
	t.Helper()
	home := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	cliDir := t.TempDir()
	cliPath := filepath.Join(cliDir, "pi")
	if err := os.WriteFile(cliPath, []byte("pi fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", cliDir)
	request := planRequest(ModeReadOnly)
	request.CanonicalCwd = workspace
	return request, time.Now()
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
