package antigravity

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// selectedAgyEnvironment is a complete caller-selected environment for
// workspace: its own HOME, trusted for that workspace, and a PATH holding only
// a fixture agy executable that differs from the ambient one. It returns the
// HOME and that executable's path.
func selectedAgyEnvironment(t *testing.T, workspace string) ([]string, string, string) {
	t.Helper()
	home, _ := profileFixture(t)
	writeFixtureSettings(t, home, workspace, "selected-model")
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cliPath := filepath.Join(directory, "agy")
	if err = os.WriteFile(cliPath, []byte("selected agy fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{"HOME=" + home, "PATH=" + directory}, home, cliPath
}

func requireSelectedAgy(t *testing.T, label string, candidate commonprovider.ProfileCandidate, home, cliPath, otherHome string) {
	t.Helper()
	if candidate.Inspection == nil {
		t.Fatalf("%s has no runtime inspection", label)
	}
	if !slices.Contains(candidate.Inspection.Environment, "HOME="+home) {
		t.Fatalf("%s did not read the expected HOME (%d entries)", label, len(candidate.Inspection.Environment))
	}
	for _, entry := range candidate.Inspection.Environment {
		if strings.Contains(entry, otherHome) {
			t.Fatalf("%s leaked an unselected HOME into %q", label, strings.SplitN(entry, "=", 2)[0])
		}
	}
	if candidate.Inspection.Executable != cliPath {
		t.Fatalf("%s executable %q want the selected PATH's %q", label, candidate.Inspection.Executable, cliPath)
	}
}

func TestFreshCandidateReadsProvidedEnvironmentNotAmbient(t *testing.T) {
	fixture := newSuccessorFixture(t, ModeWorkspaceWrite)
	ambientHome := os.Getenv("HOME")
	environment, home, cliPath := selectedAgyEnvironment(t, fixture.request.CanonicalCwd)
	candidate, err := PrepareCandidate(fixture.request, environment)
	if err != nil {
		t.Fatal(err)
	}
	requireSelectedAgy(t, "fresh candidate", candidate, home, cliPath, ambientHome)
}

func TestPrepareDoesNotResolveExecutableFromAmbientPath(t *testing.T) {
	fixture := newSuccessorFixture(t, ModeWorkspaceWrite)
	environment, _, _ := selectedAgyEnvironment(t, fixture.request.CanonicalCwd)
	withoutPath := slices.DeleteFunc(slices.Clone(environment), func(entry string) bool { return strings.HasPrefix(entry, "PATH=") })
	if _, err := PrepareCandidate(fixture.request, withoutPath); err == nil {
		t.Fatal("fresh candidate without a provided PATH found the ambient agy executable")
	}
}

func TestExistingAndSuccessorUseProvidedEnvironmentWhenNoneIsRecorded(t *testing.T) {
	fixture := newSuccessorFixture(t, ModeWorkspaceWrite)
	ambientHome := os.Getenv("HOME")
	environment, home, cliPath := selectedAgyEnvironment(t, fixture.request.CanonicalCwd)
	unrecorded := fixture.meta
	unrecorded.Environment, unrecorded.EnvironmentRecorded = nil, false
	for name, prepare := range map[string]commonprovider.PrepareExistingCandidate{
		"existing":  PrepareExistingCandidate,
		"successor": PrepareSuccessorCandidate,
	} {
		candidate, err := prepare(fixture.request, unrecorded, environment)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		requireSelectedAgy(t, name+" candidate", candidate, home, cliPath, ambientHome)
	}
}

func TestPortableExistingCandidateFallsBackToProvidedEnvironment(t *testing.T) {
	fixture := newSuccessorFixture(t, ModeWorkspaceWrite)
	ambientHome := os.Getenv("HOME")
	environment, home, cliPath := selectedAgyEnvironment(t, fixture.request.CanonicalCwd)
	candidate, err := PrepareExistingCandidate(fixture.request, task.MetaRecord{}, environment)
	if err != nil {
		t.Fatal(err)
	}
	requireSelectedAgy(t, "portable existing candidate", candidate, home, cliPath, ambientHome)
}

// The provided scope owns executable lookup, as the ambient process scope did
// before environments were explicit; the record owns only the launch
// environment of an admitted task's reconstruction.
func TestRecordedLaunchEnvironmentResolvesExecutableFromProvidedScope(t *testing.T) {
	fixture := newSuccessorFixture(t, ModeWorkspaceWrite)
	provided, providedHome, providedCLI := selectedAgyEnvironment(t, fixture.request.CanonicalCwd)
	withoutPath := fixture.meta
	withoutPath.Environment = slices.DeleteFunc(slices.Clone(fixture.meta.Environment), func(entry string) bool { return strings.HasPrefix(entry, "PATH=") })
	for recordedName, recorded := range map[string]task.MetaRecord{"recorded": fixture.meta, "recorded without PATH": withoutPath} {
		candidate, err := PrepareExistingCandidate(fixture.request, recorded, provided)
		if err != nil {
			t.Fatalf("%s: %v", recordedName, err)
		}
		requireRecordedLaunchWithProvidedExecutable(t, recordedName+" existing", candidate, recorded.Environment, providedCLI, providedHome)
		if !slices.Equal(candidate.WritableRoots, recorded.EffectiveConfig.Policy.WritableRoots) {
			t.Errorf("%s existing writable roots %q, want the recorded roots", recordedName, candidate.WritableRoots)
		}
	}
	invalid := fixture.meta
	invalid.Environment = []string{}
	providedWithoutPath := slices.DeleteFunc(slices.Clone(provided), func(entry string) bool { return strings.HasPrefix(entry, "PATH=") })
	if _, err := PrepareExistingCandidate(fixture.request, invalid, providedWithoutPath); err == nil || strings.Contains(err.Error(), "provider executable") {
		t.Fatalf("invalid recorded environment error=%v want its validation error before executable lookup", err)
	}
}

// A continuation is a new launch: the provided scope owns its values, roots,
// and executable, while the predecessor's recorded environment is only a
// compatibility input and is never launched.
func TestSuccessorLaunchesUnderProvidedScopeNotRecordedEnvironment(t *testing.T) {
	fixture := newSuccessorFixture(t, ModeWorkspaceWrite)
	provided, providedHome, providedCLI := selectedAgyEnvironment(t, fixture.request.CanonicalCwd)
	withoutPath := fixture.meta
	withoutPath.Environment = slices.DeleteFunc(slices.Clone(fixture.meta.Environment), func(entry string) bool { return strings.HasPrefix(entry, "PATH=") })
	invalid := fixture.meta
	invalid.Environment = []string{}
	for name, recorded := range map[string]task.MetaRecord{"recorded": fixture.meta, "recorded without PATH": withoutPath, "recorded empty": invalid} {
		candidate, err := PrepareSuccessorCandidate(fixture.request, recorded, provided)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if candidate.Inspection == nil || !slices.Contains(candidate.Inspection.Environment, "HOME="+providedHome) ||
			candidate.Inspection.Executable != providedCLI || candidate.Inspection.ExecutableSHA256 != fixtureSHA256(t, providedCLI) {
			t.Errorf("%s successor did not launch under the provided scope", name)
		}
	}
}

// fixtureSHA256 fingerprints a fixture executable the way static preparation
// does, without running it.
func fixtureSHA256(t *testing.T, path string) string {
	t.Helper()
	info, err := commonprovider.LocateCLIPath(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.SHA256
}

func TestRecordedEmptyEnvironmentDoesNotFallBackToProvided(t *testing.T) {
	fixture := newSuccessorFixture(t, ModeWorkspaceWrite)
	provided, _, _ := selectedAgyEnvironment(t, fixture.request.CanonicalCwd)
	recorded := fixture.meta
	recorded.Environment = []string{}
	if _, err := PrepareExistingCandidate(fixture.request, recorded, provided); err == nil {
		t.Fatal("existing with a recorded empty environment fell back to the provided one")
	}
}

// requireRecordedLaunchWithProvidedExecutable asserts that candidate launches
// with exactly the recorded environment while its executable, by path and
// fingerprint, comes from the provided scope.
func requireRecordedLaunchWithProvidedExecutable(t *testing.T, label string, candidate commonprovider.ProfileCandidate, recorded []string, providedCLI, providedHome string) {
	t.Helper()
	if candidate.Inspection == nil || !slices.Equal(candidate.Inspection.Environment, recorded) {
		t.Fatalf("%s did not launch with the recorded environment", label)
	}
	if candidate.Inspection.Executable != providedCLI || candidate.Inspection.ExecutableSHA256 != fixtureSHA256(t, providedCLI) {
		t.Fatalf("%s executable %q want the provided scope's %q", label, candidate.Inspection.Executable, providedCLI)
	}
	for _, entry := range candidate.Inspection.Environment {
		if strings.Contains(entry, providedHome) {
			t.Fatalf("%s leaked the provided scope into %q", label, strings.SplitN(entry, "=", 2)[0])
		}
	}
}
