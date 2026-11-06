package claude

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// selectedClaudeEnvironment is a complete caller-selected environment: its own
// HOME and a PATH holding only a fixture claude executable that differs from
// the ambient one. It returns the HOME and that executable's path.
func selectedClaudeEnvironment(t *testing.T) ([]string, string, string) {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cliPath := filepath.Join(directory, "claude")
	if err = os.WriteFile(cliPath, []byte("selected claude fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{"HOME=" + directory, "PATH=" + directory}, directory, cliPath
}

func requireSelectedClaude(t *testing.T, label string, candidate commonprovider.ProfileCandidate, home, cliPath, otherHome string) {
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
	fixture := newSuccessorFixture(t, Mode)
	ambientHome := os.Getenv("HOME")
	environment, home, cliPath := selectedClaudeEnvironment(t)
	candidate, err := PrepareCandidate(fixture.request, environment)
	if err != nil {
		t.Fatal(err)
	}
	requireSelectedClaude(t, "fresh candidate", candidate, home, cliPath, ambientHome)
	if admitted := requireStaticAdmission(t, candidate); !slices.Contains(admitted.Plan.Environment, "HOME="+home) || admitted.Plan.Executable != cliPath {
		t.Fatalf("admitted plan did not select the provided HOME and executable (%d entries)", len(admitted.Plan.Environment))
	}
}

func TestPrepareDoesNotResolveExecutableFromAmbientPath(t *testing.T) {
	fixture := newSuccessorFixture(t, Mode)
	environment, _, _ := selectedClaudeEnvironment(t)
	withoutPath := slices.DeleteFunc(slices.Clone(environment), func(entry string) bool { return strings.HasPrefix(entry, "PATH=") })
	if _, err := PrepareCandidate(fixture.request, withoutPath); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("fresh candidate without a provided PATH error=%v want ErrUnsupportedProfile although the ambient PATH has claude", err)
	}
}

// The provided scope owns executable lookup, as the ambient process scope did
// before environments were explicit; the record owns only the launch
// environment of its own reconstruction. A new successor launches from the
// provided scope whatever its predecessor recorded.
func TestRecordedLaunchEnvironmentResolvesExecutableFromProvidedScope(t *testing.T) {
	fixture := newSuccessorFixture(t, Mode)
	ambientHome := os.Getenv("HOME")
	provided, providedHome, providedCLI := selectedClaudeEnvironment(t)
	withoutPath := fixture.meta
	withoutPath.Environment = slices.DeleteFunc(slices.Clone(fixture.meta.Environment), func(entry string) bool { return strings.HasPrefix(entry, "PATH=") })
	for recordedName, recorded := range map[string]task.MetaRecord{"recorded": fixture.meta, "recorded without PATH": withoutPath} {
		candidate, err := PrepareExistingCandidate(fixture.request, recorded, provided)
		if err != nil {
			t.Fatalf("%s existing: %v", recordedName, err)
		}
		requireRecordedLaunchWithProvidedExecutable(t, recordedName+" existing", candidate, recorded.Environment, providedCLI, providedHome)
		successor, err := PrepareSuccessorCandidate(fixture.request, recorded, provided)
		if err != nil {
			t.Fatalf("%s successor: %v", recordedName, err)
		}
		requireSelectedClaude(t, recordedName+" successor", successor, providedHome, providedCLI, ambientHome)
	}
	invalid := fixture.meta
	invalid.Environment = []string{}
	providedWithoutPath := slices.DeleteFunc(slices.Clone(provided), func(entry string) bool { return strings.HasPrefix(entry, "PATH=") })
	if _, err := PrepareExistingCandidate(fixture.request, invalid, providedWithoutPath); !errors.Is(err, ErrUnsupportedProfile) || strings.Contains(err.Error(), "provider executable") {
		t.Fatalf("invalid recorded environment error=%v want its validation error before executable lookup", err)
	}
	if _, err := PrepareSuccessorCandidate(fixture.request, fixture.meta, providedWithoutPath); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("successor without a provided PATH error=%v want ErrUnsupportedProfile although its predecessor recorded one", err)
	}
}

func TestExistingAndSuccessorUseProvidedEnvironmentWhenNoneIsRecorded(t *testing.T) {
	fixture := newSuccessorFixture(t, Mode)
	ambientHome := os.Getenv("HOME")
	environment, home, cliPath := selectedClaudeEnvironment(t)
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
		requireSelectedClaude(t, name+" candidate", candidate, home, cliPath, ambientHome)
	}
}

func TestPortableExistingCandidateFallsBackToProvidedEnvironment(t *testing.T) {
	fixture := newSuccessorFixture(t, Mode)
	ambientHome := os.Getenv("HOME")
	environment, home, cliPath := selectedClaudeEnvironment(t)
	candidate, err := PrepareExistingCandidate(fixture.request, task.MetaRecord{}, environment)
	if err != nil {
		t.Fatal(err)
	}
	requireSelectedClaude(t, "portable existing candidate", candidate, home, cliPath, ambientHome)
}

func TestExistingKeepsRecordedEnvironmentOverProvided(t *testing.T) {
	fixture := newSuccessorFixture(t, Mode)
	provided, providedHome, _ := selectedClaudeEnvironment(t)
	recorded := fixture.meta
	candidate, err := PrepareExistingCandidate(fixture.request, recorded, provided)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Inspection == nil || !slices.Equal(candidate.Inspection.Environment, recorded.Environment) {
		t.Fatal("existing did not keep the recorded environment")
	}
	for _, entry := range candidate.Inspection.Environment {
		if strings.Contains(entry, providedHome) {
			t.Fatalf("existing leaked the provided HOME into %q", strings.SplitN(entry, "=", 2)[0])
		}
	}
}

func TestSuccessorUsesProvidedEnvironmentOverRecorded(t *testing.T) {
	fixture := newSuccessorFixture(t, Mode)
	recordedHome := os.Getenv("HOME")
	provided, providedHome, providedCLI := selectedClaudeEnvironment(t)
	candidate, err := PrepareSuccessorCandidate(fixture.request, fixture.meta, provided)
	if err != nil {
		t.Fatal(err)
	}
	requireSelectedClaude(t, "successor", candidate, providedHome, providedCLI, recordedHome)
}

func TestRecordedEmptyEnvironmentDoesNotFallBackToProvided(t *testing.T) {
	fixture := newSuccessorFixture(t, Mode)
	provided, _, _ := selectedClaudeEnvironment(t)
	recorded := fixture.meta
	recorded.Environment = []string{}
	if _, err := PrepareExistingCandidate(fixture.request, recorded, provided); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("existing with a recorded empty environment error=%v want ErrUnsupportedProfile instead of the provided HOME", err)
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
	if candidate.Inspection.Executable != providedCLI || candidate.Inspection.ExecutableSHA256 != "" {
		t.Fatalf("%s executable %q want the provided scope's %q", label, candidate.Inspection.Executable, providedCLI)
	}
	for _, entry := range candidate.Inspection.Environment {
		if strings.Contains(entry, providedHome) {
			t.Fatalf("%s leaked the provided scope into %q", label, strings.SplitN(entry, "=", 2)[0])
		}
	}
}
