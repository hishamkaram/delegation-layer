package pi

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
)

// selectedPiEnvironment is a complete caller-selected environment: its own
// HOME and a PATH holding only a fixture pi executable that differs from the
// ambient one. It returns that executable's path.
func selectedPiEnvironment(t *testing.T) ([]string, string, string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cliPath := filepath.Join(directory, "pi")
	if err = os.WriteFile(cliPath, []byte("selected pi fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{"HOME=" + home, "PATH=" + directory}, home, cliPath
}

func requireSelectedPi(t *testing.T, label string, candidate commonprovider.ProfileCandidate, home, cliPath, ambientHome string) {
	t.Helper()
	if !slices.Contains(candidate.Inspection.Environment, "HOME="+home) {
		t.Fatalf("%s did not read the provided HOME (%d entries)", label, len(candidate.Inspection.Environment))
	}
	for _, entry := range candidate.Inspection.Environment {
		if strings.Contains(entry, ambientHome) {
			t.Fatalf("%s leaked the ambient HOME into %q", label, strings.SplitN(entry, "=", 2)[0])
		}
	}
	if candidate.Inspection.Executable != cliPath {
		t.Fatalf("%s executable %q want the provided PATH's %q", label, candidate.Inspection.Executable, cliPath)
	}
}

func TestFreshCandidateReadsProvidedEnvironmentNotAmbient(t *testing.T) {
	request, _ := profileReconstructionFixture(t, ModeReadOnly)
	ambientHome := os.Getenv("HOME")
	environment, home, cliPath := selectedPiEnvironment(t)
	candidate, err := PrepareCandidate(request, environment)
	if err != nil {
		t.Fatal(err)
	}
	requireSelectedPi(t, "fresh candidate", candidate, home, cliPath, ambientHome)
}

func TestPrepareDoesNotResolveExecutableFromAmbientPath(t *testing.T) {
	request, _ := profileReconstructionFixture(t, ModeReadOnly)
	environment, _, _ := selectedPiEnvironment(t)
	withoutPath := slices.DeleteFunc(slices.Clone(environment), func(entry string) bool { return strings.HasPrefix(entry, "PATH=") })
	if _, err := PrepareCandidate(request, withoutPath); !errors.Is(err, ErrUnsupportedProfile) {
		t.Fatalf("fresh candidate without a provided PATH error=%v want ErrUnsupportedProfile although the ambient PATH has pi", err)
	}
}

func TestExistingAndSuccessorUseProvidedEnvironmentWhenNoneIsRecorded(t *testing.T) {
	fixture := newPiSuccessorFixture(t, ModeReadOnly)
	ambientHome := os.Getenv("HOME")
	environment, home, cliPath := selectedPiEnvironment(t)
	predecessor := priorReferenceForMode(fixture.request.Mode)
	meta := fixture.meta(predecessor)

	existing, err := fixture.catalog.ExistingCandidate(fixture.request, meta, environment)
	if err != nil {
		t.Fatal(err)
	}
	requireSelectedPi(t, "existing candidate", existing, home, cliPath, ambientHome)
	successor, err := fixture.catalog.SuccessorCandidate(fixture.request, meta, environment)
	if err != nil {
		t.Fatal(err)
	}
	requireSelectedPi(t, "successor candidate", successor, home, cliPath, ambientHome)
}

// The provided scope owns executable lookup, as the ambient process scope did
// before environments were explicit; the record owns only the launch
// environment of the task it admitted. Reconstruction of that task runs under
// the caller's scope while the record saved another one.
func TestRecordedLaunchEnvironmentResolvesExecutableFromProvidedScope(t *testing.T) {
	fixture := newPiSuccessorFixture(t, ModeReadOnly)
	provided, providedHome, providedCLI := selectedPiEnvironment(t)
	recorded, recordedHome, _ := selectedPiEnvironment(t)
	withoutPath := slices.DeleteFunc(slices.Clone(recorded), func(entry string) bool { return strings.HasPrefix(entry, "PATH=") })
	for recordedName, environment := range map[string][]string{"recorded": recorded, "recorded without PATH": withoutPath} {
		meta := fixture.meta(priorReferenceForMode(fixture.request.Mode))
		meta.Environment, meta.EnvironmentRecorded = environment, true
		candidate, err := fixture.catalog.ExistingCandidate(fixture.request, meta, provided)
		if err != nil {
			t.Fatalf("%s existing: %v", recordedName, err)
		}
		requireSelectedPi(t, recordedName+" existing candidate", candidate, recordedHome, providedCLI, providedHome)
		if candidate.Inspection.ExecutableSHA256 != "" {
			t.Fatalf("%s existing executable fingerprint is not the provided scope's", recordedName)
		}
	}
	meta := fixture.meta(priorReferenceForMode(fixture.request.Mode))
	meta.Environment, meta.EnvironmentRecorded = []string{}, true
	providedWithoutPath := slices.DeleteFunc(slices.Clone(provided), func(entry string) bool { return strings.HasPrefix(entry, "PATH=") })
	if _, err := PrepareExistingCandidate(fixture.request, meta, providedWithoutPath); err == nil || strings.Contains(err.Error(), "provider executable") {
		t.Fatalf("invalid recorded environment error=%v want its validation error before executable lookup", err)
	}
}

// A new successor runs entirely under the caller's scope, whatever the
// predecessor recorded: its launch environment, roots, and executable are the
// caller's, so an ordinary --resume-task successor does not inherit the
// predecessor's HOME.
func TestSuccessorLaunchesUnderProvidedScopeDespiteRecordedEnvironment(t *testing.T) {
	fixture := newPiSuccessorFixture(t, ModeReadOnly)
	provided, providedHome, providedCLI := selectedPiEnvironment(t)
	recorded, recordedHome, _ := selectedPiEnvironment(t)
	withoutPath := slices.DeleteFunc(slices.Clone(recorded), func(entry string) bool { return strings.HasPrefix(entry, "PATH=") })
	for recordedName, environment := range map[string][]string{"recorded": recorded, "recorded without PATH": withoutPath} {
		meta := fixture.meta(priorReferenceForMode(fixture.request.Mode))
		meta.Environment, meta.EnvironmentRecorded = environment, true
		candidate, err := fixture.catalog.SuccessorCandidate(fixture.request, meta, provided)
		if err != nil {
			t.Fatalf("%s successor: %v", recordedName, err)
		}
		requireSelectedPi(t, recordedName+" successor candidate", candidate, providedHome, providedCLI, recordedHome)
		if candidate.Inspection.ExecutableSHA256 != "" {
			t.Fatalf("%s successor executable fingerprint is not the provided scope's", recordedName)
		}
	}
}

func TestRecordedEmptyEnvironmentDoesNotFallBackToProvided(t *testing.T) {
	fixture := newPiSuccessorFixture(t, ModeReadOnly)
	provided, _, _ := selectedPiEnvironment(t)
	for name, recorded := range map[string][]string{"nil": nil, "empty": {}} {
		meta := fixture.meta(priorReferenceForMode(fixture.request.Mode))
		meta.Environment, meta.EnvironmentRecorded = recorded, true
		if _, err := PrepareExistingCandidate(fixture.request, meta, provided); err == nil {
			t.Fatalf("recorded %s environment fell back to the provided one", name)
		}
	}
}
