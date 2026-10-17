package supervisor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func DiscoverControlOwnerExecutable() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	self = canonicalExecutablePath(self)
	candidate := self
	base := filepath.Base(self)
	if base != stateRunnerName && !strings.HasPrefix(base, stateRunnerName+"-") {
		candidate = managedRunnerFromExecutable(self)
	}
	if !filepath.IsAbs(candidate) || filepath.Clean(candidate) != candidate {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil || resolved != candidate {
		return ""
	}
	info, err := os.Stat(candidate)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return ""
	}
	return candidate
}

func ResolveInitialExecutable(initialSupervisorExecutable string) (string, error) {
	path := initialSupervisorExecutable
	if path == "" {
		var err error
		path, err = exec.LookPath("pueue")
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

func resolveBundledExecutable(initialSupervisorExecutable string, name string) (string, error) {
	directory, err := bundledSupervisorDirectory(initialSupervisorExecutable)
	if err != nil {
		return "", err
	}
	return resolveSupervisorExecutable(filepath.Join(directory, name), name)
}

func resolveBundledExecutables(initialSupervisorExecutable string) (client, daemon string, resultErr error) {
	return resolveBundledExecutablesWithPath(initialSupervisorExecutable, true)
}

func resolveBundledExecutablesWithPath(initialSupervisorExecutable string, allowPath bool) (client, daemon string, resultErr error) {
	directory, err := bundledSupervisorDirectory(initialSupervisorExecutable)
	if err != nil {
		return "", "", err
	}
	client, daemon, found, statePairErr := ResolveStateSupervisorPair(directory)
	if found {
		return client, daemon, statePairErr
	}
	client, daemon, clientErr := resolveSupervisorPair(directory)
	if clientErr == nil {
		return client, daemon, nil
	}
	if initialSupervisorExecutable != "" || bundledSupervisorPairHasEntry(directory) {
		return "", "", clientErr
	}
	client, daemon, found, libexecErr := resolveBundledLibexecPair(directory)
	if found {
		return client, daemon, libexecErr
	}
	if !allowPath {
		return "", "", clientErr
	}

	pathClient, pathErr := exec.LookPath("pueue")
	if pathErr != nil {
		return "", "", clientErr
	}
	client, err = resolveSupervisorExecutable(pathClient, "pueue")
	if err != nil {
		return "", "", err
	}
	daemon, err = resolveSupervisorExecutable(filepath.Join(filepath.Dir(client), "pueued"), "pueued")
	if err != nil {
		return "", "", fmt.Errorf("%w: pueue and pueued must be installed as one executable pair: %w", pueue.ErrConfiguration, err)
	}
	return client, daemon, nil
}

func ResolvePrivateSupervisorExecutables(root string, initialSupervisorExecutable string) (client, daemon string, resultErr error) {
	stateClient, stateDaemon, found, stateErr := ResolveStateSupervisorPair(filepath.Join(root, ".supervisor"))
	if stateErr != nil {
		return "", "", stateErr
	}
	if found {
		return stateClient, stateDaemon, nil
	}
	return resolveBundledExecutables(initialSupervisorExecutable)
}

// resolveCurrentPrivateSupervisorExecutables selects the pair shipped by the
// active installation for successor inventory and explicit migration work. It
// does not select the ordinary active generation: pueue's completed published
// binding remains authoritative until canonical migration publishes a
// replacement, while a fresh root may bootstrap the staged pair.
func ResolveCurrentPrivateSupervisorExecutables(root string, initialSupervisorExecutable string) (client, daemon string, resultErr error) {
	client, daemon, bundledErr := resolveBundledExecutablesWithPath(initialSupervisorExecutable, false)
	if bundledErr == nil {
		return client, daemon, nil
	}
	stateClient, stateDaemon, found, stateErr := ResolveStateSupervisorPair(filepath.Join(root, ".supervisor"))
	if stateErr != nil {
		return "", "", errors.Join(bundledErr, stateErr)
	}
	if found {
		return stateClient, stateDaemon, nil
	}
	return resolveBundledExecutables(initialSupervisorExecutable)
}

func resolveSupervisorPair(directory string) (client, daemon string, resultErr error) {
	client, err := resolveSupervisorExecutable(filepath.Join(directory, "pueue"), "pueue")
	if err != nil {
		return "", "", err
	}
	daemon, err = resolveSupervisorExecutable(filepath.Join(directory, "pueued"), "pueued")
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
	return filepath.Join(filepath.Dir(canonicalExecutablePath(self)), "delegate-run")
}

func resolveSupervisorExecutable(candidate, name string) (string, error) {
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
	if resolved != raw {
		return "", pueue.ErrConfiguration
	}
	info, err := os.Stat(raw)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", pueue.ErrConfiguration
	}
	return raw, nil
}

func ResolveTaskRunner(raw string, runnerExecutable string, meta *task.MetaRecord) (string, error) {
	if raw == "" && meta != nil && meta.RunnerExecutable != "" {
		raw = meta.RunnerExecutable
	}
	return ResolveRunner(raw, runnerExecutable)
}
