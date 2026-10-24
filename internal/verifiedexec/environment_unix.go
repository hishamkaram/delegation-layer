//go:build unix

package verifiedexec

import "path/filepath"

// TemporaryRoot is the parent a launch with this environment allocates its
// scratch directories under: the selected TMPDIR, else the native default.
// It never reads the process environment.
func TemporaryRoot(environment []string) string {
	return environmentTemporaryRoot(environment)
}

// environmentTemporaryRoot selects TMPDIR without reading the process
// environment. The fallback is os.TempDir's native Linux/macOS default.
func environmentTemporaryRoot(environment []string) string {
	if root := environmentValue(environment, "TMPDIR"); root != "" {
		return root
	}
	return "/tmp"
}

func shouldMirrorParent(path string, environment []string) bool {
	path = filepath.Clean(path)
	if path == string(filepath.Separator) {
		return false
	}
	temporaryRoot := environmentTemporaryRoot(environment)
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
