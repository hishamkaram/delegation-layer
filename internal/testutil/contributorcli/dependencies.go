package contributorcli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/app"
)

const (
	testSupervisorEnvironment = "DELEGATE_TEST_PUEUE"
	delegateRunnerName        = "delegate-run"
	// A missing explicit supervisor must fail dispatch resolution instead of
	// falling back to a developer's ambient pueue executable. Read-only
	// observation remains available with an empty environment.
	missingSupervisorPath = "/__contributorcli_missing__/pueue"
)

var errInvalidComposition = errors.New("invalid contributor CLI composition")

// NewDependencies composes the normal application dependencies for the
// current wrapper executable. It does not read a sibling harness config, so
// collect and providers remain observational when provider fixtures are
// absent.
func NewDependencies() (app.Dependencies, error) {
	return NewDependenciesForExecutable("")
}

// NewDependenciesForExecutable is the testable composition boundary. The
// runner is always the sibling delegate-run path; app.Run and app.RunRunner
// perform the final executable checks when a launch actually needs it.
func NewDependenciesForExecutable(executable string) (app.Dependencies, error) {
	canonical, err := canonicalExecutable(executable)
	if err != nil {
		return app.Dependencies{}, err
	}
	catalog, err := NewCatalog()
	if err != nil {
		return app.Dependencies{}, fmt.Errorf("constructing contributor catalog: %w", err)
	}
	supervisor, err := configuredSupervisor()
	if err != nil {
		return app.Dependencies{}, err
	}
	return app.Dependencies{
		Catalog:                     catalog,
		InitialSupervisorExecutable: supervisor,
		RunnerExecutable:            filepath.Join(filepath.Dir(canonical), delegateRunnerName),
	}, nil
}

// Run is the command entry point for the contributor acceptance wrapper.
func Run(args []string, stdout, stderr io.Writer) int {
	deps, err := NewDependencies()
	if err != nil {
		return writeCompositionError(stderr, err)
	}
	return app.Run(args, stdout, stderr, deps)
}

// RunRunner is the runner entry point for the contributor acceptance wrapper.
func RunRunner(args []string, stdout, stderr io.Writer) int {
	deps, err := NewDependencies()
	if err != nil {
		return writeCompositionError(stderr, err)
	}
	return app.RunRunner(args, stdout, stderr, deps)
}

func configuredSupervisor() (string, error) {
	raw := os.Getenv(testSupervisorEnvironment)
	if raw == "" {
		return missingSupervisorPath, nil
	}
	if !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
		return "", fmt.Errorf("%s must be an absolute clean path: %w", testSupervisorEnvironment, errInvalidComposition)
	}
	return raw, nil // availability is checked only by operations that need the supervisor
}

func canonicalExecutable(raw string) (string, error) {
	if raw == "" {
		var err error
		raw, err = os.Executable()
		if err != nil {
			return "", fmt.Errorf("resolving contributor wrapper executable: %w", err)
		}
	}
	return validateExecutablePath(raw, "contributor wrapper executable")
}

func validateExecutablePath(raw, label string) (string, error) {
	if !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
		return "", fmt.Errorf("%s must be an absolute canonical executable: %w", label, errInvalidComposition)
	}
	resolved, err := filepath.EvalSymlinks(raw)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", label, err)
	}
	if filepath.Clean(resolved) != raw {
		return "", fmt.Errorf("%s must not be a symlink: %w", label, errInvalidComposition)
	}
	info, err := os.Stat(raw)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", label, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s is not an executable regular file: %w", label, errInvalidComposition)
	}
	return raw, nil
}

func writeCompositionError(stderr io.Writer, err error) int {
	if stderr == nil {
		return 1
	}
	if _, writeErr := fmt.Fprintf(stderr, "error: %v\n", err); writeErr != nil {
		return 1
	}
	return 1
}
