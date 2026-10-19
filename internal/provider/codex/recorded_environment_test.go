package codex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// launchHome is one caller's launch selectors. The saved task and the command
// that reconstructs it use different values for every selector.
type launchHome struct {
	home, codexHome, dataHome, tmpDir string
}

func newLaunchHome(t *testing.T, name string) launchHome {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	selectors := launchHome{
		home: filepath.Join(root, name+"-home"), codexHome: filepath.Join(root, name+"-codex"),
		dataHome: filepath.Join(root, name+"-data"), tmpDir: filepath.Join(root, name+"-tmp"),
	}
	for _, path := range []string{selectors.home, selectors.codexHome, selectors.dataHome, selectors.tmpDir} {
		if err = os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return selectors
}

func (h launchHome) writeAuth(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.codexHome, "auth.json"), marshalAuth(t, fixtureAuth(t, time.Now(), "pro")), 0o600); err != nil {
		t.Fatal(err)
	}
}

// inheritedLaunchKeys are the variables the native Codex profile can retain.
// PATH is excluded because each fixture installs its own fake CLI directory.
var inheritedLaunchKeys = []string{
	"HOME", "CODEX_HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR", "TMP", "TEMP", "__CF_USER_TEXT_ENCODING",
	"XDG_CONFIG_HOME", "XDG_CONFIG_DIRS", "XDG_DATA_HOME", "XDG_DATA_DIRS", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS",
	"GOCACHE", "GOMODCACHE", "CARGO_HOME", "RUSTUP_HOME", "GRADLE_USER_HOME", "NPM_CONFIG_CACHE", "GOPATH",
}

// apply makes h the only source of retained launch variables, so the
// environment a test records or reads never carries the host's own values.
func (h launchHome) apply(t *testing.T) {
	t.Helper()
	for _, key := range inheritedLaunchKeys {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", h.home)
	t.Setenv("CODEX_HOME", h.codexHome)
	t.Setenv("XDG_DATA_HOME", h.dataHome)
	t.Setenv("TMPDIR", h.tmpDir)
}

func (h launchHome) paths() []string {
	return []string{h.home, h.codexHome, h.dataHome, h.tmpDir}
}

// summarizeValues reports only a count so a failure never prints environment
// entries or host paths; the equality assertion itself carries the comparison.
func summarizeValues(values []string) string {
	return fmt.Sprintf("%d entries", len(values))
}

// requireNoPath reports the leaking selector's position in paths and the entry
// index, never the entry itself.
func requireNoPath(t *testing.T, label string, values []string, paths []string) {
	t.Helper()
	for valueIndex, value := range values {
		for pathIndex, path := range paths {
			if strings.Contains(value, path) {
				t.Fatalf("%s entry %d leaked ambient selector #%d", label, valueIndex, pathIndex)
			}
		}
	}
}

// profileDifferences names the profile components that differ from the
// admitted profile and recorded environment.
func profileDifferences(rebuilt, admitted commonprovider.PreparedProfile, recorded []string) []string {
	var differences []string
	for name, same := range map[string]bool{
		"environment": slices.Equal(rebuilt.Plan.Environment, recorded), "arguments": slices.Equal(rebuilt.Plan.Arguments, admitted.Plan.Arguments),
		"writable roots": slices.Equal(rebuilt.WritableRoots, admitted.WritableRoots), "effective config": task.CompareEffectiveConfigs(rebuilt.Effective, admitted.Effective),
	} {
		if !same {
			differences = append(differences, name)
		}
	}
	slices.Sort(differences)
	return differences
}

type recordedEnvironmentCase struct {
	name   string
	mode   string
	review bool
}

func TestExistingNativeCandidateUsesRecordedEnvironmentNotAmbient(t *testing.T) {
	for _, testCase := range []recordedEnvironmentCase{
		{name: "read-only exec", mode: Mode},
		{name: "workspace-write exec", mode: WorkspaceWriteMode},
		{name: "read-only review", mode: Mode, review: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request, _ := profileReconstructionFixture(t)
			request.Mode, request.RequestedConfig.Permission = testCase.mode, testCase.mode
			prepare, prepareExisting := PrepareCandidate, PrepareExistingCandidate
			if testCase.review {
				request.Provider = ReviewProvider
				request.RequestedConfig.ReviewTarget = &task.ReviewTarget{Kind: task.ReviewTargetUncommitted}
				prepare, prepareExisting = PrepareReviewCandidate, PrepareExistingReviewCandidate
			}
			saved, ambient := newLaunchHome(t, "saved"), newLaunchHome(t, "ambient")
			saved.apply(t)
			candidate, err := prepare(request)
			if err != nil {
				t.Fatal(err)
			}
			admitted, err := candidate.Admit()
			if err != nil {
				t.Fatal(err)
			}
			meta := task.MetaRecord{
				EffectiveConfig: admitted.Effective, RuntimePreflightSHA256: admitted.RuntimePreflightSHA256,
				Environment: slices.Clone(admitted.Plan.Environment), EnvironmentRecorded: true, Predicate: admitted.Plan.Predicate,
				OutputArtifacts: slices.Clone(admitted.Plan.OutputArtifacts), OutputWriterContract: admitted.Plan.OutputWriterContract,
			}
			ambient.apply(t)
			assertRecordedReconstruction(t, request, meta, admitted, prepareExisting, saved, ambient)
		})
	}
}

func assertRecordedReconstruction(t *testing.T, request task.TaskRecord, meta task.MetaRecord, admitted commonprovider.PreparedProfile, prepareExisting commonprovider.PrepareExistingCandidate, saved, ambient launchHome) {
	t.Helper()
	existing, err := prepareExisting(request, meta)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(existing.WritableRoots, admitted.WritableRoots) {
		t.Fatalf("candidate roots %s want recorded %s", summarizeValues(existing.WritableRoots), summarizeValues(admitted.WritableRoots))
	}
	if !slices.Equal(existing.Inspection.Environment, meta.Environment) {
		t.Fatalf("inspection environment %s want recorded %s", summarizeValues(existing.Inspection.Environment), summarizeValues(meta.Environment))
	}
	rebuilt, err := existing.Admit()
	if err != nil {
		t.Fatal(err)
	}
	if differences := profileDifferences(rebuilt, admitted, meta.Environment); len(differences) > 0 {
		t.Fatalf("rebuilt profile differs from admitted in %q", differences)
	}
	requireNoPath(t, "launch environment", rebuilt.Plan.Environment, ambient.paths())
	requireNoPath(t, "writable roots", rebuilt.WritableRoots, ambient.paths())
	for key, value := range map[string]string{"HOME": saved.home, "CODEX_HOME": saved.codexHome, "XDG_DATA_HOME": saved.dataHome} {
		if !slices.Contains(rebuilt.Plan.Environment, key+"="+value) {
			t.Fatalf("recorded selector %s missing from launch environment (%s)", key, summarizeValues(rebuilt.Plan.Environment))
		}
	}
	if err = rebuilt.MatchesPendingRuntimeRefresh(request, meta); err != nil {
		t.Fatalf("pending reconstruction no longer matches the admitted record: %v", err)
	}
}

func TestExistingFinalizedProfileMatchesRecordUnderDifferentAmbientEnvironment(t *testing.T) {
	request, now := profileReconstructionFixture(t)
	saved, ambient := newLaunchHome(t, "saved"), newLaunchHome(t, "ambient")
	saved.apply(t)
	original := finalizeProfile(t, mustPrepareCandidate(t, request), now)
	meta := profileMeta(original)
	meta.EnvironmentRecorded = true
	ambient.apply(t)
	existing, err := PrepareExistingCandidate(request, meta)
	if err != nil {
		t.Fatal(err)
	}
	replay := finalizeProfile(t, existing, now)
	if err = replay.Matches(request, meta); err != nil {
		t.Fatalf("finalized reconstruction no longer matches the record: %v", err)
	}
	requireNoPath(t, "finalized environment", replay.Plan.Environment, ambient.paths())
}

func TestExistingLegacyCandidateUsesRecordedEnvironmentNotAmbient(t *testing.T) {
	request, now := profileReconstructionFixture(t)
	saved, ambient := newLaunchHome(t, "saved"), newLaunchHome(t, "ambient")
	saved.writeAuth(t)
	ambient.writeAuth(t)
	meta := task.MetaRecord{
		EffectiveConfig: task.EffectiveConfig{Policy: &task.PolicyDetails{ProfileRevision: ProfileRevision}},
		Predicate:       LegacyReference(),
	}
	saved.apply(t)
	original, err := PrepareExistingCandidate(request, meta)
	if err != nil {
		t.Fatal(err)
	}
	legacy := finalizeProfile(t, original, now)
	meta = profileMeta(legacy)
	meta.EnvironmentRecorded = true
	ambient.apply(t)
	existing, err := PrepareExistingCandidate(request, meta)
	if err != nil {
		t.Fatal(err)
	}
	replay := finalizeProfile(t, existing, now)
	if err = replay.Matches(request, meta); err != nil {
		t.Fatalf("legacy reconstruction no longer matches the record: %v", err)
	}
	requireNoPath(t, "legacy environment", replay.Plan.Environment, ambient.paths())
	if !slices.Equal(replay.Plan.Environment, meta.Environment) {
		t.Fatalf("legacy environment %s want recorded %s", summarizeValues(replay.Plan.Environment), summarizeValues(meta.Environment))
	}
}

func TestExistingCandidateFallsBackToAmbientOnlyWithoutRecordedEnvironment(t *testing.T) {
	request, _ := profileReconstructionFixture(t)
	ambient := newLaunchHome(t, "ambient")
	ambient.apply(t)
	meta := task.MetaRecord{
		EffectiveConfig:        task.EffectiveConfig{Policy: &task.PolicyDetails{ProfileRevision: commonprovider.NativeProfileRevision}},
		RuntimePreflightSHA256: testSpecHash, Predicate: Reference(),
	}
	candidate, err := PrepareExistingCandidate(request, meta)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := candidate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(profile.Plan.Environment, "HOME="+ambient.home) || !slices.Contains(profile.Plan.Environment, "CODEX_HOME="+ambient.codexHome) {
		t.Fatalf("legacy record without an environment must read the ambient HOME and CODEX_HOME (%s)", summarizeValues(profile.Plan.Environment))
	}
}

func TestExistingCandidateFailsClosedForRecordedEmptyEnvironment(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		environment []string
	}{
		{name: "nil slice with flag", environment: nil},
		{name: "empty slice with flag", environment: []string{}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request, _ := profileReconstructionFixture(t)
			newLaunchHome(t, "ambient").apply(t)
			meta := task.MetaRecord{
				EffectiveConfig:        task.EffectiveConfig{Policy: &task.PolicyDetails{ProfileRevision: commonprovider.NativeProfileRevision}},
				RuntimePreflightSHA256: testSpecHash, Predicate: Reference(),
				Environment: testCase.environment, EnvironmentRecorded: true,
			}
			if _, err := PrepareExistingCandidate(request, meta); !errors.Is(err, ErrUnsupportedProfile) {
				t.Fatalf("recorded empty environment error=%v want ErrUnsupportedProfile instead of the caller's HOME", err)
			}
		})
	}
}

func TestFreshCandidateReadsAmbientEnvironment(t *testing.T) {
	request, _ := profileReconstructionFixture(t)
	ambient := newLaunchHome(t, "ambient")
	ambient.apply(t)
	candidate := mustPrepareCandidate(t, request)
	profile, err := candidate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(profile.Plan.Environment, "HOME="+ambient.home) || !slices.Contains(profile.Plan.Environment, "CODEX_HOME="+ambient.codexHome) {
		t.Fatalf("fresh admission must read the caller HOME and CODEX_HOME (%s)", summarizeValues(profile.Plan.Environment))
	}
}
