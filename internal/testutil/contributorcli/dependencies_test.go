package contributorcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hishamkaram/delegation-layer/internal/app"
)

func TestCatalogCompositionRunsProvidersWithoutSupervisorConfig(t *testing.T) {
	t.Setenv(testSupervisorEnvironment, "")
	executable, err := testExecutable()
	if err != nil {
		t.Fatal(err)
	}
	deps, err := NewDependenciesForExecutable(executable)
	if err != nil {
		t.Fatal(err)
	}
	if deps.Catalog.IsZero() {
		t.Fatal("contributor catalog was not initialized")
	}
	if deps.InitialSupervisorExecutable != missingSupervisorPath {
		t.Fatalf("missing supervisor did not remain explicit: %q", deps.InitialSupervisorExecutable)
	}
	wantRunner := filepath.Join(filepath.Dir(executable), delegateRunnerName)
	if deps.RunnerExecutable != wantRunner {
		t.Fatalf("runner path = %q, want %q", deps.RunnerExecutable, wantRunner)
	}

	var stdout, stderr bytes.Buffer
	if code := app.Run([]string{"providers", "--json"}, &stdout, &stderr, deps); code != 0 {
		t.Fatalf("providers returned %d: stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response app.ProviderResponse
	if err = json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("providers response was not JSON: %v (%q)", err, stdout.String())
	}
	if response.SchemaVersion != app.OutputSchemaVersion || len(response.Providers) != len(deps.Catalog.Descriptions()) || response.Error != "" {
		t.Fatalf("unexpected provider response: %+v", response)
	}
}

func TestCompositionUsesExplicitSupervisorExecutable(t *testing.T) {
	executable, err := testExecutable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(testSupervisorEnvironment, executable)
	deps, err := NewDependenciesForExecutable(executable)
	if err != nil {
		t.Fatal(err)
	}
	if deps.InitialSupervisorExecutable != executable {
		t.Fatalf("supervisor = %q, want %q", deps.InitialSupervisorExecutable, executable)
	}
}

func TestCompositionRejectsInvalidExplicitSupervisorPath(t *testing.T) {
	executable, err := testExecutable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(testSupervisorEnvironment, "relative/pueue")
	if _, err = NewDependenciesForExecutable(executable); !errors.Is(err, errInvalidComposition) {
		t.Fatalf("invalid supervisor path was not rejected explicitly: %v", err)
	}
}

func testExecutable() (string, error) {
	raw, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(raw)
}

func TestCompositionDoesNotProbeMissingSupervisor(t *testing.T) {
	executable, err := testExecutable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(testSupervisorEnvironment, filepath.Join(t.TempDir(), "absent-pueue"))
	deps, err := NewDependenciesForExecutable(executable)
	if err != nil {
		t.Fatalf("observational composition probed supervisor: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := app.Run([]string{"providers", "--json"}, &stdout, &stderr, deps); code != 0 {
		t.Fatalf("discovery required a supervisor: %q", stderr.String())
	}
}
