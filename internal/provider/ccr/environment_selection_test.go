package ccr

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/config"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// selectedCCREnvironment is a complete caller-selected environment: its own
// HOME and a PATH holding only a fixture ccr executable that differs from the
// ambient one. It returns the HOME and that executable's path.
func selectedCCREnvironment(t *testing.T) ([]string, string, string) {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cliPath := filepath.Join(directory, "ccr")
	if err = os.WriteFile(cliPath, []byte("selected ccr fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{"HOME=" + directory, "PATH=" + directory}, directory, cliPath
}

func requireSelectedCCR(t *testing.T, label string, candidate commonprovider.ProfileCandidate, home, cliPath, otherHome string) {
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
	request := successorRequest(t, config.ModeReadOnly)
	ambientHome := os.Getenv("HOME")
	environment, home, cliPath := selectedCCREnvironment(t)
	candidate, err := PrepareCandidate(request, environment)
	if err != nil {
		t.Fatal(err)
	}
	requireSelectedCCR(t, "fresh candidate", candidate, home, cliPath, ambientHome)
}

func TestPrepareDoesNotResolveExecutableFromAmbientPath(t *testing.T) {
	request := successorRequest(t, config.ModeReadOnly)
	environment, _, _ := selectedCCREnvironment(t)
	withoutPath := slices.DeleteFunc(slices.Clone(environment), func(entry string) bool { return strings.HasPrefix(entry, "PATH=") })
	if _, err := PrepareCandidate(request, withoutPath); err == nil {
		t.Fatal("fresh candidate without a provided PATH found the ambient ccr executable")
	}
}

// The provided scope owns executable lookup, as the ambient process scope did
// before environments were explicit; the record owns only the reconstructed
// launch environment. An ordinary --resume-task successor runs under the
// caller's scope and launch environment while its predecessor recorded another.
func TestRecordedLaunchEnvironmentResolvesExecutableFromProvidedScope(t *testing.T) {
	request := successorRequest(t, config.ModeReadOnly)
	provided, providedHome, providedCLI := selectedCCREnvironment(t)
	recorded := successorMeta(request, preAttributionReference(config.ModeReadOnly, true))
	withoutPath := recorded
	withoutPath.Environment = slices.DeleteFunc(slices.Clone(recorded.Environment), func(entry string) bool { return strings.HasPrefix(entry, "PATH=") })
	for recordedName, meta := range map[string]task.MetaRecord{"recorded": recorded, "recorded without PATH": withoutPath} {
		for name, launch := range map[string]struct {
			prepare         commonprovider.PrepareExistingCandidate
			home, otherHome string
		}{
			"existing":  {PrepareExistingCandidate, request.CanonicalCwd, providedHome},
			"successor": {PrepareSuccessorCandidate, providedHome, request.CanonicalCwd},
		} {
			candidate, err := launch.prepare(request, meta, provided)
			if err != nil {
				t.Fatalf("%s %s: %v", recordedName, name, err)
			}
			requireSelectedCCR(t, recordedName+" "+name+" candidate", candidate, launch.home, providedCLI, launch.otherHome)
			if candidate.Inspection.ExecutableSHA256 != fixtureSHA256(t, providedCLI) {
				t.Fatalf("%s %s executable fingerprint is not the provided scope's", recordedName, name)
			}
		}
	}
	invalid := recorded
	invalid.Environment = []string{}
	providedWithoutPath := slices.DeleteFunc(slices.Clone(provided), func(entry string) bool { return strings.HasPrefix(entry, "PATH=") })
	if _, err := PrepareExistingCandidate(request, invalid, providedWithoutPath); err == nil || strings.Contains(err.Error(), "provider executable") {
		t.Fatalf("invalid recorded environment error=%v want its validation error before executable lookup", err)
	}
	// A successor ignores the recorded environment and looks ccr up only in the
	// provided scope, which here has no PATH.
	if _, err := PrepareSuccessorCandidate(request, invalid, providedWithoutPath); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("provided environment without PATH error=%v want %v", err, exec.ErrNotFound)
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

func TestExistingAndSuccessorNeverFallBackToProvidedEnvironment(t *testing.T) {
	request := successorRequest(t, config.ModeReadOnly)
	provided, _, _ := selectedCCREnvironment(t)
	for name, prepare := range map[string]commonprovider.PrepareExistingCandidate{
		"existing":  PrepareExistingCandidate,
		"successor": PrepareSuccessorCandidate,
	} {
		unrecorded := task.MetaRecord{Predicate: preAttributionReference(config.ModeReadOnly, true)}
		if _, err := prepare(request, unrecorded, provided); !errors.Is(err, task.ErrIdentityMismatch) {
			t.Fatalf("%s without a recorded environment error=%v want %v", name, err, task.ErrIdentityMismatch)
		}
	}
}
