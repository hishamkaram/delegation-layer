package pi

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

func TestProfileReconstructionRetainsNativeAndLegacyBindings(t *testing.T) {
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		t.Run("native/"+mode, func(t *testing.T) {
			testNativeProfileReconstruction(t, mode)
		})
	}
	t.Run("legacy", testLegacyProfileReconstruction)
}

func TestNativeReadOnlyProfilePinsSettingsSessionDirectory(t *testing.T) {
	home := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(filepath.Join(workspace, ".pi"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	cliDir := t.TempDir()
	if writeErr := os.WriteFile(filepath.Join(cliDir, "pi"), []byte("pi fixture"), 0o700); writeErr != nil {
		t.Fatal(writeErr)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", cliDir)
	request := planRequest(ModeReadOnly)
	request.CanonicalCwd = workspace
	firstSession, err := config.CanonicalizePath(filepath.Join(home, ".pi", "agent", "sessions", "first"))
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(filepath.Join(workspace, ".pi", "settings.json"), []byte(`{"sessionDir":"`+firstSession+`"}`), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	first := finalizeProfile(t, mustPrepareCandidate(t, request), time.Now())
	if !slices.Contains(first.Plan.Environment, piSessionDirEnv+"="+firstSession) {
		t.Fatalf("resolved session directory was not pinned: %q", first.Plan.Environment)
	}

	secondSession := filepath.Join(home, ".pi", "agent", "sessions", "second")
	if writeErr := os.WriteFile(filepath.Join(workspace, ".pi", "settings.json"), []byte(`{"sessionDir":"`+secondSession+`"}`), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	replayCandidate, err := PrepareExistingCandidate(request, task.MetaRecord{
		EffectiveConfig:     first.Effective,
		Environment:         first.Plan.Environment,
		EnvironmentRecorded: true,
		Predicate:           first.Plan.Predicate,
	})
	if err != nil {
		t.Fatal(err)
	}
	replay := finalizeProfile(t, replayCandidate, time.Now())
	if !slices.Contains(replay.Plan.Environment, piSessionDirEnv+"="+firstSession) || slices.Contains(replay.Plan.Environment, piSessionDirEnv+"="+secondSession) {
		t.Fatalf("replay did not retain admitted session directory: %q", replay.Plan.Environment)
	}
	if !slices.Equal(replay.Plan.Environment, first.Plan.Environment) || !slices.Equal(replay.Plan.Arguments, first.Plan.Arguments) || !task.CompareEffectiveConfigs(replay.Effective, first.Effective) {
		t.Fatalf("replay changed the admitted launch contract: environment=%q/%q arguments=%q/%q policy=%+v/%+v", replay.Plan.Environment, first.Plan.Environment, replay.Plan.Arguments, first.Plan.Arguments, *replay.Effective.Policy, *first.Effective.Policy)
	}
}

func testNativeProfileReconstruction(t *testing.T, mode string) {
	t.Helper()
	request, now := profileReconstructionFixture(t, mode)
	nativeProfile := finalizeProfile(t, mustPrepareCandidate(t, request), now)
	requireNativeProfile(t, nativeProfile, mode)

	nativeReplayCandidate, err := PrepareExistingCandidate(request, task.MetaRecord{EffectiveConfig: nativeProfile.Effective})
	if err != nil {
		t.Fatal(err)
	}
	nativeReplay := finalizeProfile(t, nativeReplayCandidate, now)
	if !task.CompareEffectiveConfigs(nativeProfile.Effective, nativeReplay.Effective) {
		t.Fatalf("native reconstruction changed effective config: replay=%+v", nativeReplay.Effective)
	}
	if !slices.Equal(nativeProfile.Plan.Arguments, nativeReplay.Plan.Arguments) || !nativeProfile.Plan.Predicate.Equal(nativeReplay.Plan.Predicate) {
		t.Fatalf("native reconstruction changed launch binding: args=%q/%q predicate=%+v/%+v", nativeProfile.Plan.Arguments, nativeReplay.Plan.Arguments, nativeProfile.Plan.Predicate, nativeReplay.Plan.Predicate)
	}
	if mode == ModeReadOnly {
		historicalCandidate, historicalErr := PrepareExistingCandidate(request, task.MetaRecord{
			EffectiveConfig: nativeProfile.Effective,
			Predicate:       readOnlyV2Reference(),
		})
		if historicalErr != nil {
			t.Fatal(historicalErr)
		}
		historicalProfile := finalizeProfile(t, historicalCandidate, now)
		if !historicalProfile.Plan.Predicate.Equal(readOnlyV2Reference()) {
			t.Fatalf("native read-only v2 reconstruction changed predicate=%+v", historicalProfile.Plan.Predicate)
		}
	}
	historical := readOnlyV3Reference()
	if mode == ModeWorkspaceWrite {
		historical = workspaceWriteV3Reference()
	}
	historicalCandidate, err := PrepareExistingCandidate(request, task.MetaRecord{
		EffectiveConfig: nativeProfile.Effective,
		Predicate:       historical,
	})
	if err != nil {
		t.Fatal(err)
	}
	historicalProfile := finalizeProfile(t, historicalCandidate, now)
	historicalArguments, err := jsonArgumentsForProfile(request, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !historicalProfile.Plan.Predicate.Equal(historical) || !slices.Equal(historicalProfile.Plan.Arguments, historicalArguments) {
		t.Fatalf("native %s v3 reconstruction changed launch binding: args=%q predicate=%+v", mode, historicalProfile.Plan.Arguments, historicalProfile.Plan.Predicate)
	}
}

func testLegacyProfileReconstruction(t *testing.T) {
	t.Helper()
	request, now := profileReconstructionFixture(t, ModeReadOnly)
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

func TestProfileReconstructionPreservesPreUpgradePredicate(t *testing.T) {
	for _, reference := range []task.PredicateRef{readOnlyV5Reference(), readOnlyV4Reference(), workspaceWriteV7Reference(), workspaceWriteV5Reference(), workspaceWriteV4Reference()} {
		t.Run(reference.Mode, func(t *testing.T) {
			request, now := profileReconstructionFixture(t, reference.Mode)
			current := finalizeProfile(t, mustPrepareCandidate(t, request), now)
			candidate, err := PrepareExistingCandidate(request, task.MetaRecord{Predicate: reference, EffectiveConfig: current.Effective})
			if err != nil {
				t.Fatal(err)
			}
			rebuilt := finalizeProfile(t, candidate, now)
			historicalArguments, err := jsonArgumentsForProfile(request, true, false)
			if err != nil {
				t.Fatal(err)
			}
			if !rebuilt.Plan.Predicate.Equal(reference) || !slices.Equal(rebuilt.Plan.Arguments, historicalArguments) || !task.CompareEffectiveConfigs(rebuilt.Effective, current.Effective) {
				t.Fatalf("existing task contract changed: %+v", rebuilt)
			}
		})
	}
}

func TestWorkspaceWriteV7ReconstructionRetainsRuntimeCapability(t *testing.T) {
	request, now := profileReconstructionFixture(t, ModeWorkspaceWrite)
	current := finalizeProfile(t, mustPrepareCandidate(t, request), now)
	candidate, err := PrepareExistingCandidate(request, task.MetaRecord{
		Predicate:       workspaceWriteV7Reference(),
		EffectiveConfig: current.Effective,
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Inspection == nil || candidate.Inspection.Runtime == nil {
		t.Fatalf("workspace-write v7 inspection=%+v", candidate.Inspection)
	}
	wantFlags := []string{"--mode", "--model", "--thinking", "--session"}
	if !slices.Equal(candidate.Inspection.Runtime.RequiredFlags, wantFlags) || slices.Contains(candidate.Inspection.Runtime.RequiredFlags, "--tools") {
		t.Fatalf("workspace-write v7 requirements=%q", candidate.Inspection.Runtime.RequiredFlags)
	}
	rebuilt := finalizeProfile(t, candidate, now)
	if !rebuilt.Plan.Predicate.Equal(workspaceWriteV7Reference()) || slices.Contains(rebuilt.Plan.Arguments, "--tools") {
		t.Fatalf("workspace-write v7 reconstruction changed launch contract: %+v", rebuilt.Plan)
	}
}

func TestWorkspaceHistoricalRuntimeRequirementsMatchReconstructedArguments(t *testing.T) {
	request, now := profileReconstructionFixture(t, ModeWorkspaceWrite)
	currentCandidate := mustPrepareCandidate(t, request)
	current := finalizeProfile(t, currentCandidate, now)
	currentFlags := []string{"--mode", "--tools", "--model", "--thinking", "--session"}
	currentDigest := requireRuntimeProbeDefinition(t, currentCandidate.Inspection, currentFlags)
	cases := []struct {
		name                   string
		reference              task.PredicateRef
		wantFlags              []string
		wantToolsFlag          bool
		wantCurrentProbeDigest bool
	}{
		{
			name:                   "v3",
			reference:              workspaceWriteV3Reference(),
			wantFlags:              currentFlags,
			wantToolsFlag:          false,
			wantCurrentProbeDigest: true,
		},
		{
			name:                   "v4",
			reference:              workspaceWriteV4Reference(),
			wantFlags:              currentFlags,
			wantToolsFlag:          false,
			wantCurrentProbeDigest: true,
		},
		{
			name:                   "v5",
			reference:              workspaceWriteV5Reference(),
			wantFlags:              currentFlags,
			wantToolsFlag:          false,
			wantCurrentProbeDigest: true,
		},
		{
			name:          "v7",
			reference:     workspaceWriteV7Reference(),
			wantFlags:     []string{"--mode", "--model", "--thinking", "--session"},
			wantToolsFlag: false,
		},
		{
			name:                   "current",
			reference:              ReferenceForMode(ModeWorkspaceWrite),
			wantFlags:              currentFlags,
			wantToolsFlag:          true,
			wantCurrentProbeDigest: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			candidate, err := PrepareExistingCandidate(request, task.MetaRecord{
				EffectiveConfig: current.Effective,
				Predicate:       testCase.reference,
			})
			if err != nil {
				t.Fatal(err)
			}
			gotDigest := requireRuntimeProbeDefinition(t, candidate.Inspection, testCase.wantFlags)
			if (gotDigest == currentDigest) != testCase.wantCurrentProbeDigest {
				t.Fatalf("runtime definition digest=%s matches current=%t want match=%t", gotDigest, gotDigest == currentDigest, testCase.wantCurrentProbeDigest)
			}

			rebuilt := finalizeProfile(t, candidate, now)
			if !rebuilt.Plan.Predicate.Equal(testCase.reference) {
				t.Fatalf("reconstructed predicate=%+v want=%+v", rebuilt.Plan.Predicate, testCase.reference)
			}
			if got := slices.Contains(rebuilt.Plan.Arguments, "--tools"); got != testCase.wantToolsFlag {
				t.Fatalf("reconstructed argv=%q contains --tools=%t want=%t", rebuilt.Plan.Arguments, got, testCase.wantToolsFlag)
			}
		})
	}
}

func profileReconstructionFixture(t *testing.T, mode string) (task.TaskRecord, time.Time) {
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
	request := planRequest(mode)
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

func requireNativeProfile(t *testing.T, profile commonprovider.PreparedProfile, mode string) {
	t.Helper()
	policy := profile.Effective.Policy
	if policy == nil || policy.ProfileRevision != commonprovider.NativeProfileRevision || len(policy.Sources) != 0 {
		t.Fatalf("native effective policy=%+v", policy)
	}
	if profile.Effective.Containment != mode {
		t.Fatalf("native containment=%q want=%q", profile.Effective.Containment, mode)
	}
	wantApproval := "read-only"
	if mode == ModeWorkspaceWrite {
		wantApproval = "provider-native"
	}
	if profile.Effective.Approval != wantApproval {
		t.Fatalf("native approval=%q want=%q", profile.Effective.Approval, wantApproval)
	}
	if !profile.Plan.Predicate.Equal(ReferenceForMode(mode)) {
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
