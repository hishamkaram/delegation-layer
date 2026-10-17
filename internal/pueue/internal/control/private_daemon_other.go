//go:build !linux && !darwin

package control

import "os/exec"

func configurePrivateDaemonCommand(_ *exec.Cmd) error {
	return ErrConfiguration
}
