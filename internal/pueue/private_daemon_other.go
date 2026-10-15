//go:build !linux && !darwin

package pueue

import "os/exec"

func configurePrivateDaemonCommand(_ *exec.Cmd) error {
	return ErrConfiguration
}
