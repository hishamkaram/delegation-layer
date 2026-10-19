package opencode

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
	home, configHome, dataHome, tmpDir string
}

func newLaunchHome(t *testing.T, name string) launchHome {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	selectors := launchHome{
		home: filepath.Join(root, name+"-home"), configHome: filepath.Join(root, name+"-config"),
		dataHome: filepath.Join(root, name+"-data"), tmpDir: filepath.Join(root, name+"-tmp"),
	}
	for _, path := range selectors.paths() {
		if err = os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return selectors
}

// inheritedLaunchKeys are the variables the native OpenCode profile can retain.
// PATH is excluded because each fixture installs its own fake CLI directory.
var inheritedLaunchKeys = []string{
	"HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR", "TMP", "TEMP", "__CF_USER_TEXT_ENCODING",
	"XDG_CONFIG_HOME", "XDG_CONFIG_DIRS", "XDG_DATA_HOME", "XDG_DATA_DIRS", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
	"DBUS_SESSION_BUS_ADDRESS", "SSH_AUTH_SOCK",
	"OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_TUI_CONFIG", "OPENCODE_PERMISSION", "OPENCODE_AUTO_SHARE",
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
	t.Setenv("XDG_CONFIG_HOME", h.configHome)
	t.Setenv("XDG_DATA_HOME", h.dataHome)
	t.Setenv("TMPDIR", h.tmpDir)
}

func (h launchHome) paths() []string {
	return []string{h.home, h.configHome, h.dataHome, h.tmpDir}
}

func (h launchHome) selectors() []string {
	return []string{"HOME=" + h.home, "XDG_CONFIG_HOME=" + h.configHome, "XDG_DATA_HOME=" + h.dataHome, "TMPDIR=" + h.tmpDir}
}

// summarizeValues reports only a count so a failure never prints environment
// entries or host paths; the equality assertion itself carries the comparison.
func summarizeValues(values []string) string {
	return fmt.Sprintf("%d entries", len(values))
}

// requireNoPath reports the leaking selector's position in paths and the entry
// index, never the entry itself.
func requireNoPath(t *testing.T, label string, values, paths []string) {
	t.Helper()
	for valueIndex, value := range values {
		for pathIndex, path := range paths {
			if strings.Contains(value, path) {
				t.Fatalf("%s entry %d leaked ambient selector #%d", label, valueIndex, pathIndex)
			}
		}
	}
}

// requireSelectors checks each expected KEY=value entry and reports only the
// missing key.
func requireSelectors(t *testing.T, label string, environment, selectors []string) {
	t.Helper()
	for _, selector := range selectors {
		if !slices.Contains(environment, selector) {
			key, _, _ := strings.Cut(selector, "=")
			t.Fatalf("%s: selector %s missing from environment (%s)", label, key, summarizeValues(environment))
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

// recordedEnvironmentFixture installs a fake provider CLI on PATH so every
// reconstruction resolves the same executable regardless of the caller's HOME.
func recordedEnvironmentFixture(t *testing.T, mode string) task.TaskRecord {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "opencode"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	request := profileRequest(mode)
	request.CanonicalCwd = directory
	return request
}

func TestExistingNativeCandidateUsesRecordedEnvironmentNotAmbient(t *testing.T) {
	for _, mode := range []string{ModeReadOnly, ModeWorkspaceWrite} {
		t.Run(mode, func(t *testing.T) {
			request := recordedEnvironmentFixture(t, mode)
			saved, ambient := newLaunchHome(t, "saved"), newLaunchHome(t, "ambient")
			saved.apply(t)
			candidate, err := PrepareCandidate(request)
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
			}
			ambient.apply(t)
			assertRecordedReconstruction(t, request, meta, admitted, saved, ambient)
		})
	}
}

func assertRecordedReconstruction(t *testing.T, request task.TaskRecord, meta task.MetaRecord, admitted commonprovider.PreparedProfile, saved, ambient launchHome) {
	t.Helper()
	existing, err := PrepareExistingCandidate(request, meta)
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
	requireNoPath(t, "inspection environment", existing.Inspection.Environment, ambient.paths())
	requireSelectors(t, "recorded launch environment", rebuilt.Plan.Environment, saved.selectors())
	if err = rebuilt.MatchesPendingRuntimeRefresh(request, meta); err != nil {
		t.Fatalf("pending reconstruction no longer matches the admitted record: %v", err)
	}
}

func TestExistingFinalizedProfileMatchesRecordUnderDifferentAmbientEnvironment(t *testing.T) {
	request := recordedEnvironmentFixture(t, ModeWorkspaceWrite)
	saved, ambient := newLaunchHome(t, "saved"), newLaunchHome(t, "ambient")
	saved.apply(t)
	candidate, err := PrepareCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := commonprovider.EncodeInspectionFacts(commonprovider.RuntimeFacts{
		Executable: candidate.Inspection.Executable, SHA256: candidate.Inspection.ExecutableSHA256, Version: "fixture",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	original, err := candidate.Finalize(facts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	meta := task.MetaRecord{
		EffectiveConfig: original.Effective, ProviderExecutable: original.Plan.Executable, ProviderVersion: original.ObservedVersion,
		Environment: slices.Clone(original.Plan.Environment), EnvironmentRecorded: true, Predicate: original.Plan.Predicate,
	}
	ambient.apply(t)
	existing, err := PrepareExistingCandidate(request, meta)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := existing.Finalize(facts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = replay.Matches(request, meta); err != nil {
		t.Fatalf("finalized reconstruction no longer matches the record: %v", err)
	}
	requireNoPath(t, "finalized environment", replay.Plan.Environment, ambient.paths())
}

func TestExistingLegacyCandidateUsesRecordedEnvironmentNotAmbient(t *testing.T) {
	request := recordedEnvironmentFixture(t, ModeReadOnly)
	saved, ambient := newLaunchHome(t, "saved"), newLaunchHome(t, "ambient")
	recorded := append(saved.selectors(), "PATH="+os.Getenv("PATH"))
	slices.Sort(recorded)
	ambient.apply(t)
	candidate, err := PrepareExistingCandidate(request, task.MetaRecord{Environment: recorded, EnvironmentRecorded: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(candidate.Inspection.Environment, recorded) {
		t.Fatalf("legacy inspection environment %s want recorded %s", summarizeValues(candidate.Inspection.Environment), summarizeValues(recorded))
	}
	requireNoPath(t, "legacy inspection environment", candidate.Inspection.Environment, ambient.paths())
}

func TestExistingCandidateFallsBackToAmbientOnlyWithoutRecordedEnvironment(t *testing.T) {
	for _, native := range []bool{true, false} {
		name := "legacy"
		meta := task.MetaRecord{}
		if native {
			name = "native"
			meta = task.MetaRecord{EffectiveConfig: task.EffectiveConfig{Policy: &task.PolicyDetails{ProfileRevision: commonprovider.NativeProfileRevision}}}
		}
		t.Run(name, func(t *testing.T) {
			request := recordedEnvironmentFixture(t, ModeReadOnly)
			ambient := newLaunchHome(t, "ambient")
			ambient.apply(t)
			candidate, err := PrepareExistingCandidate(request, meta)
			if err != nil {
				t.Fatal(err)
			}
			requireSelectors(t, "record without an environment must read the ambient one", candidate.Inspection.Environment, []string{"HOME=" + ambient.home})
		})
	}
}

func TestExistingCandidateFailsClosedForRecordedEmptyEnvironment(t *testing.T) {
	native := task.EffectiveConfig{Policy: &task.PolicyDetails{ProfileRevision: commonprovider.NativeProfileRevision}}
	for _, testCase := range []struct {
		name        string
		effective   task.EffectiveConfig
		environment []string
	}{
		{name: "native nil slice with flag", effective: native},
		{name: "native empty slice with flag", effective: native, environment: []string{}},
		{name: "legacy nil slice with flag"},
		{name: "legacy empty slice with flag", environment: []string{}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := recordedEnvironmentFixture(t, ModeReadOnly)
			newLaunchHome(t, "ambient").apply(t)
			meta := task.MetaRecord{EffectiveConfig: testCase.effective, Environment: testCase.environment, EnvironmentRecorded: true}
			if _, err := PrepareExistingCandidate(request, meta); !errors.Is(err, ErrUnsupportedProfile) {
				t.Fatalf("recorded empty environment error=%v want ErrUnsupportedProfile instead of the caller's HOME", err)
			}
		})
	}
}

func TestFreshCandidateReadsAmbientEnvironment(t *testing.T) {
	request := recordedEnvironmentFixture(t, ModeReadOnly)
	ambient := newLaunchHome(t, "ambient")
	ambient.apply(t)
	candidate, err := PrepareCandidate(request)
	if err != nil {
		t.Fatal(err)
	}
	requireSelectors(t, "fresh admission must read the caller environment", candidate.Inspection.Environment, ambient.selectors())
}
