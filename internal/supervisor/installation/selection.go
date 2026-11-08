package installation

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

// DiscoverControlOwnerExecutable selects the trusted default role. A selected
// explicit path remains nonempty on failure so the command owner can report it;
// optional default discovery declines only a missing installation or old copy.
func DiscoverControlOwnerExecutable(configuredRole string) string {
	if configuredRole != "" {
		if selected, err := ResolveRunner(configuredRole, ""); err == nil {
			return selected
		}
		return configuredRole
	}
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	image, _ := debug.ReadBuildInfo()
	return discoverControlOwnerForImage(canonicalExecutablePath(self), image)
}

func discoverControlOwnerForImage(self string, image *debug.BuildInfo) string {
	if pueue.IsLegacyStateRunnerPath(filepath.Dir(filepath.Dir(self)), self) {
		return ""
	}
	candidate := managedRunnerFromExecutable(self)
	if ManagedRunnerBuildInfo(image) {
		candidate = self
	}
	selected, err := ResolveRunner(candidate, "")
	if err == nil {
		return selected
	}
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	// A malformed or inaccessible selected role must not enable direct launch.
	// Control rechecks metadata before duplication and permit consumption.
	return candidate
}

// ExecutableLookup locates a supervisor pair that no binding records: the
// explicit initial executable, else the bundled installation, else PATH from
// Environment. Environment is the launch scope captured by composition; nil
// is the standalone process environment.
type ExecutableLookup struct {
	InitialExecutable string
	Environment       []string
	// Resident composition owns this immutable input from its accepted startup
	// artifacts. Canonical bootstrap and explicit migration consume it under target locks;
	// recorded generations and migration phases retain their existing owner.
	// The zero value leaves standalone selection unchanged. This is not native
	// launch authority.
	AcceptedBootstrapPair BootstrapPair
}

type BootstrapPair struct {
	Client string
	Daemon string
}

func (lookup ExecutableLookup) lookPath(name string) (string, error) {
	if lookup.Environment == nil {
		return exec.LookPath(name)
	}
	return verifiedexec.LookPath(name, lookup.Environment)
}

func ResolveInitialExecutable(lookup ExecutableLookup) (string, error) {
	path := lookup.InitialExecutable
	if path == "" {
		var err error
		path, err = lookup.lookPath("pueue")
		if err != nil {
			return "", fmt.Errorf("resolving pueue executable: %w", err)
		}
	}
	if !filepath.IsAbs(path) {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		path = absolute
	}
	return path, nil
}

// ResolvePrivateSupervisorExecutables selects installed fixed roles in place.
// The state root owns bindings and configuration, never executable inventories.
func ResolvePrivateSupervisorExecutables(lookup ExecutableLookup) (client, daemon string, resultErr error) {
	if lookup.AcceptedBootstrapPair != (BootstrapPair{}) {
		return resolveAcceptedSupervisorPair(lookup.AcceptedBootstrapPair)
	}
	directory, err := bundledSupervisorDirectory(lookup.InitialExecutable)
	if err != nil {
		return "", "", err
	}
	client, daemon, clientErr := resolveSupervisorPair(directory)
	if clientErr == nil {
		return client, daemon, nil
	}
	clientErr = classifyUnavailableBundledPair(clientErr)
	if lookup.InitialExecutable != "" || bundledSupervisorPairHasEntry(directory) {
		return "", "", clientErr
	}
	client, daemon, found, libexecErr := resolveBundledLibexecPair(directory)
	if found {
		return client, daemon, classifyUnavailableBundledPair(libexecErr)
	}

	pathClient, pathErr := lookup.lookPath("pueue")
	if pathErr != nil {
		if !errors.Is(pathErr, exec.ErrNotFound) && !errors.Is(pathErr, os.ErrNotExist) {
			return "", "", errors.Join(clientErr, pathErr)
		}
		return "", "", clientErr
	}
	client, err = ResolveSupervisorExecutable(pathClient, "pueue")
	if err != nil {
		return "", "", err
	}
	daemon, err = ResolveSupervisorExecutable(filepath.Join(filepath.Dir(client), "pueued"), "pueued")
	if err != nil {
		return "", "", fmt.Errorf("%w: pueue and pueued must be installed as one executable pair: %w", pueue.ErrConfiguration, err)
	}
	return client, daemon, nil
}

func resolveSupervisorPair(directory string) (client, daemon string, resultErr error) {
	client, err := ResolveSupervisorExecutable(filepath.Join(directory, "pueue"), "pueue")
	if err != nil {
		return "", "", err
	}
	daemon, err = ResolveSupervisorExecutable(filepath.Join(directory, "pueued"), "pueued")
	if err != nil {
		return "", "", fmt.Errorf("%w: pueue and pueued must be installed as one executable pair: %w", pueue.ErrConfiguration, err)
	}
	return client, daemon, nil
}

func bundledSupervisorPairHasEntry(directory string) bool {
	for _, name := range []string{"pueue", "pueued"} {
		if _, err := os.Lstat(filepath.Join(directory, name)); err == nil || !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

func resolveBundledLibexecPair(binaryDirectory string) (client, daemon string, found bool, resultErr error) {
	directory := filepath.Clean(filepath.Join(binaryDirectory, "..", "libexec"))
	client, daemon, err := resolveSupervisorPair(directory)
	if err == nil {
		return client, daemon, true, nil
	}
	if bundledSupervisorPairHasEntry(directory) {
		return "", "", true, err
	}
	return "", "", false, nil
}

func bundledSupervisorDirectory(initialSupervisorExecutable string) (string, error) {
	if initialSupervisorExecutable != "" {
		return filepath.Dir(initialSupervisorExecutable), nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	self = canonicalExecutablePath(self)
	return filepath.Dir(self), nil
}

func canonicalExecutablePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

func managedRunnerFromExecutable(self string) string {
	return filepath.Join(filepath.Dir(canonicalExecutablePath(self)), managedRunnerName)
}

// ResolveSupervisorExecutable validates one selected installed role without fallback.
func ResolveSupervisorExecutable(candidate, name string) (string, error) {
	path, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("%w: %s executable is unavailable: %w", pueue.ErrConfiguration, name, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%w: inspect %s executable: %w", pueue.ErrConfiguration, name, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%w: %s executable must be executable regular file", pueue.ErrConfiguration, name)
	}
	return path, nil
}

func ResolveRunner(raw string, runnerExecutable string) (string, error) {
	if raw == "" {
		raw = runnerExecutable
	}
	if raw == "" {
		self, err := os.Executable()
		if err != nil {
			return "", err
		}
		raw = managedRunnerFromExecutable(self)
	}
	if !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
		return "", pueue.ErrConfiguration
	}
	resolved, err := filepath.EvalSymlinks(raw)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", pueue.ErrConfiguration
	}
	return resolved, nil
}

func ResolveTaskRunner(raw string, runnerExecutable string, meta *task.MetaRecord) (string, error) {
	if raw != "" || meta == nil || meta.RunnerExecutable == "" {
		return ResolveRunner(raw, runnerExecutable)
	}
	recorded := meta.RunnerExecutable
	root := filepath.Dir(filepath.Dir(recorded))
	if meta.RunnerOwnership != task.RunnerOwnershipCustom && pueue.IsLegacyStateRunnerPath(root, recorded) {
		return ResolveRunner("", runnerExecutable)
	}
	selected, err := ResolveRunner(recorded, "")
	if err == nil || !errors.Is(err, os.ErrNotExist) || meta.RunnerOwnership != task.RunnerOwnershipManaged {
		return selected, err
	}
	return ResolveRunner("", runnerExecutable)
}

// The accepted context stores resolved native paths. Re-observation may refuse
// their replacement by an alias, but must never select a sibling client instead.
func resolveAcceptedSupervisorPair(accepted BootstrapPair) (string, string, error) {
	for _, path := range []string{accepted.Client, accepted.Daemon} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return "", "", pueue.ErrConfiguration
		}
	}
	client, clientErr := ResolveSupervisorExecutable(accepted.Client, "pueue")
	daemon, daemonErr := ResolveSupervisorExecutable(accepted.Daemon, "pueued")
	if err := errors.Join(clientErr, daemonErr); err != nil {
		return "", "", err
	}
	if client != accepted.Client || daemon != accepted.Daemon {
		return "", "", errors.Join(pueue.ErrConfiguration, ErrSupervisorSelectionChanged)
	}
	return client, daemon, nil
}
