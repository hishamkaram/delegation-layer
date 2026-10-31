//go:build unix

package verifiedexec

import "path/filepath"

// launchPath resolves path the way the child will: a relative path names
// something below its launch directory, and a relative launch directory is
// relative to this process, as exec.Cmd.Dir is. It is the one place a
// launch-relative path becomes absolute; it never reads the process environment
// and does not follow symlinks.
func launchPath(directory, path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	return filepath.Abs(filepath.Join(directory, path))
}

// TemporaryRoot is the parent a launch with this environment allocates its
// scratch directories under: the selected TMPDIR, else the native default.
// It never reads the process environment.
func TemporaryRoot(environment []string) string {
	return environmentTemporaryRoot(environment)
}

// environmentTemporaryRoot selects TMPDIR without reading the process
// environment. The fallback is os.TempDir's native Linux/macOS default.
func environmentTemporaryRoot(environment []string) string {
	if root := EnvironmentValue(environment, "TMPDIR"); root != "" {
		return root
	}
	return "/tmp"
}

// shouldMirrorParent reports whether the ancestor path of an entrypoint is
// mirrored into a view. temporaryRoot is the launch-resolved TMPDIR boundary,
// absolute, never the raw environment spelling.
func shouldMirrorParent(path, temporaryRoot string) bool {
	path = filepath.Clean(path)
	if path == string(filepath.Separator) {
		return false
	}
	if canonical, err := filepath.EvalSymlinks(temporaryRoot); err == nil && path == filepath.Clean(canonical) {
		return false
	}
	for _, sharedRoot := range []string{
		filepath.Clean(temporaryRoot),
		"/tmp",
		"/private/tmp",
		"/var/tmp",
		"/private/var/tmp",
		"/var/folders",
		"/private/var/folders",
		"/bin",
		"/sbin",
		"/usr",
		"/usr/bin",
		"/usr/sbin",
		"/System",
		"/private",
	} {
		if path == sharedRoot {
			return false
		}
	}
	return true
}
