//go:build unix

package verifiedexec

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
