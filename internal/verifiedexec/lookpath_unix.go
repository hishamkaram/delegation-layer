//go:build unix

package verifiedexec

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// LookPath resolves a program using only the supplied environment. Explicit
// paths keep Go's native executable checks and errors. Search candidates are
// also checked by Go without invoking its ambient PATH search; relative search
// results retain exec.ErrDot unless the selected GODEBUG disables it with
// execerrdot=0, as Go documents for its own lookup. Missing and empty PATH do
// not search any directory. The first PATH entry wins, matching recorded
// interpreter lookup.
func LookPath(name string, environment []string) (string, error) {
	switch name {
	case "", ".", "..":
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
	if strings.ContainsRune(name, filepath.Separator) {
		return exec.LookPath(name)
	}
	errDotDisabled := execErrDotDisabled(environment)
	for _, directory := range filepath.SplitList(environmentValue(environment, "PATH")) {
		candidate := filepath.Join(directory, name)
		probe := candidate
		if !filepath.IsAbs(probe) {
			// An explicit path bypasses Go's process-wide PATH lookup even when
			// filepath.Join cleaned an empty or dot directory out of candidate.
			probe = "." + string(filepath.Separator) + probe
		}
		if _, err := exec.LookPath(probe); err != nil {
			continue
		}
		if !filepath.IsAbs(candidate) && !errDotDisabled {
			return candidate, &exec.Error{Name: name, Err: exec.ErrDot}
		}
		return candidate, nil
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// execErrDotDisabled reports Go's documented execerrdot=0 opt-out from the
// selected GODEBUG only; the process GODEBUG is never read. As in Go, the last
// execerrdot setting wins and only the exact value 0 disables the check.
func execErrDotDisabled(environment []string) bool {
	disabled := false
	for _, setting := range strings.Split(environmentValue(environment, "GODEBUG"), ",") {
		if name, value, found := strings.Cut(setting, "="); found && name == "execerrdot" {
			disabled = value == "0"
		}
	}
	return disabled
}
