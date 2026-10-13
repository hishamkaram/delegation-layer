//go:build !linux && !darwin

package lifetime

import "os/exec"

// ConfigureIndependentSession is unsupported on platforms without the Unix
// process-session primitive used by Delegate's lifecycle owner.
func ConfigureIndependentSession(*exec.Cmd) error {
	return ErrUnsupported
}
