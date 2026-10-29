//go:build unix && !linux && !darwin

package verifiedexec

import "errors"

var errPoolUnsupported = errors.New("custody pool is unsupported on this platform")

// filesystemForbidsExec has no verified primitive off Linux and macOS, so the
// pool refuses to run rather than assume the mount allows execution.
func filesystemForbidsExec(string) (bool, error) { return false, errPoolUnsupported }
